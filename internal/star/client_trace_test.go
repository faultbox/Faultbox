package star

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// RFC-055 §5.5 — the trace half. A client call must land in the event log
// as the *client's* activity: its own lane (service = client name), its own
// vector-clock participant, and events an anchor can match.

// startOrdersServer runs a bare HTTP server that serves the ordersSpec
// contract, plus one deliberately non-conforming route. Using net/http
// directly (rather than mock_service) keeps this test focused on the
// client's own behaviour.
func startOrdersServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/500"):
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"orders down"}`))
		case strings.HasSuffix(r.URL.Path, "/degraded"):
			// 200, but eta is null — the schema declares it
			// required and non-nullable.
			_, _ = w.Write([]byte(`{"id": 7, "eta": null}`))
		default:
			_, _ = w.Write([]byte(`{"id": 42, "eta": "12m"}`))
		}
	})
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"echo": r.Header.Get("X-Client")})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write(body)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// clientRuntime loads a spec whose target service points at addr. The
// service is declared with a binary that is never started — client calls
// dial the address directly, so no process lifecycle is involved.
func clientRuntime(t *testing.T, addr, clientKwargs string) *Runtime {
	t.Helper()
	dir := t.TempDir()
	contract := filepath.Join(dir, "orders.yaml")
	if err := os.WriteFile(contract, []byte(clientOrdersSpec), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	rt := New(testLogger())
	src := `
orders = service("orders", interface("public", "http", ` + portStr + `), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + contract + `"` + clientKwargs + `)
`
	if err := rt.LoadString("client_trace.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	return rt
}

// callOp invokes a generated operation directly, bypassing test-body
// execution so the assertions can focus on the event log.
func callOp(t *testing.T, rt *Runtime, clientName, opName string, args map[string]any) *Response {
	t.Helper()
	c, ok := rt.Client(clientName)
	if !ok {
		t.Fatalf("client %q not found", clientName)
	}
	op, ok := c.Table.Lookup(opName)
	if !ok {
		t.Fatalf("operation %q not found", opName)
	}
	v, err := rt.executeClientCall(nil, c, op, args)
	if err != nil {
		t.Fatalf("executeClientCall(%s): %v", opName, err)
	}
	resp, ok := v.(*Response)
	if !ok {
		t.Fatalf("call returned %T, want *Response", v)
	}
	return resp
}

// eventsOfType returns every event of a given type from the log.
func eventsOfType(rt *Runtime, typ string) []Event {
	var out []Event
	for _, ev := range rt.events.Events() {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestClientCall_EmitsClientLaneEvents(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, headers = {"X-Client": "ios/4.2"}`)

	resp := callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": int64(42)})
	if !resp.Ok || resp.Status != 200 {
		t.Fatalf("response = status %d ok=%v error=%q", resp.Status, resp.Ok, resp.Error)
	}
	if resp.Client != "mobile-app" || resp.Operation != "get_order" {
		t.Errorf("response provenance = client %q operation %q", resp.Client, resp.Operation)
	}
	if !resp.ContractOk {
		t.Errorf("contract_ok = false with validation off; want true (\"not checked\" reads as no violation)")
	}

	calls := eventsOfType(rt, "client_call")
	returns := eventsOfType(rt, "client_return")
	if len(calls) != 1 || len(returns) != 1 {
		t.Fatalf("got %d client_call / %d client_return events, want 1 each", len(calls), len(returns))
	}

	call, ret := calls[0], returns[0]

	// The event's Service is what the report keys lanes on and what the
	// event log advances a vector clock for. It must be the client.
	if call.Service != "mobile-app" {
		t.Errorf("client_call service = %q, want mobile-app — the client needs its own lane", call.Service)
	}
	if ret.Service != "mobile-app" {
		t.Errorf("client_return service = %q, want mobile-app", ret.Service)
	}
	// The dotted form names the callee; the event's Service names the
	// caller. Between them a reader gets both ends of the edge.
	if call.EventType != "client_call.orders" {
		t.Errorf("client_call event_type = %q, want client_call.orders", call.EventType)
	}
	if ret.EventType != "client_return.orders" {
		t.Errorf("client_return event_type = %q, want client_return.orders", ret.EventType)
	}

	wantCallFields := map[string]string{
		"client":    "mobile-app",
		"target":    "orders",
		"operation": "get_order",
		"method":    "GET",
		"path":      "/orders/42",
		"params":    "order_id=42",
		"interface": "public",
		"protocol":  "http",
	}
	for k, want := range wantCallFields {
		if got := call.Fields[k]; got != want {
			t.Errorf("client_call field %s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasSuffix(call.Fields["contract"], "orders.yaml@2.1.0") {
		t.Errorf("client_call contract = %q, want the versioned contract identity", call.Fields["contract"])
	}

	for k, want := range map[string]string{
		"client":      "mobile-app",
		"target":      "orders",
		"operation":   "get_order",
		"status_code": "200",
		"success":     "true",
		"contract_ok": "true",
	} {
		if got := ret.Fields[k]; got != want {
			t.Errorf("client_return field %s = %q, want %q", k, got, want)
		}
	}

	// Vector clocks: the client is a participant, and by the time it
	// returns it has observed both the test driver and the target.
	if call.VectorClock["mobile-app"] == 0 {
		t.Errorf("client_call vector clock has no mobile-app entry: %v", call.VectorClock)
	}
	if ret.VectorClock["mobile-app"] <= call.VectorClock["mobile-app"] {
		t.Errorf("client clock did not advance between call and return: %v → %v",
			call.VectorClock, ret.VectorClock)
	}
}

func TestClientCall_TwoClientsAreDistinctActors(t *testing.T) {
	addr := startOrdersServer(t)
	dir := t.TempDir()
	contract := filepath.Join(dir, "orders.yaml")
	if err := os.WriteFile(contract, []byte(clientOrdersSpec), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(addr)

	rt := New(testLogger())
	if err := rt.LoadString("two_clients.star", `
orders = service("orders", interface("public", "http", `+portStr+`), image = "orders:1")
mobile  = client("mobile-app",  target = orders.public, openapi = "`+contract+`",
                 headers = {"X-Client": "ios"})
partner = client("partner-api", target = orders.public, openapi = "`+contract+`",
                 headers = {"X-Client": "partner"})
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": int64(1)})
	callOp(t, rt, "partner-api", "get_order", map[string]any{"order_id": int64(2)})

	lanes := map[string]int{}
	for _, ev := range eventsOfType(rt, "client_call") {
		lanes[ev.Service]++
	}
	if len(lanes) != 2 || lanes["mobile-app"] != 1 || lanes["partner-api"] != 1 {
		t.Errorf("client_call lanes = %v, want one call each on mobile-app and partner-api", lanes)
	}
}

func TestClientCall_ContractViolationIsRecordedNotRaised(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "response"`)

	// The /degraded path returns 200 with a null in a required field.
	resp := callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": "degraded"})
	if !resp.Ok || resp.Status != 200 {
		t.Fatalf("expected a 200 with a bad payload, got status %d ok=%v", resp.Status, resp.Ok)
	}
	if resp.ContractOk {
		t.Error("contract_ok = true; the null eta violates the declared schema")
	}
	if resp.ContractError == "" {
		t.Error("contract_error is empty on a violation")
	}

	violations := eventsOfType(rt, "contract_violation")
	if len(violations) != 1 {
		t.Fatalf("got %d contract_violation events, want 1", len(violations))
	}
	v := violations[0]
	if v.Service != "mobile-app" {
		t.Errorf("contract_violation service = %q, want the client's lane", v.Service)
	}
	for _, k := range []string{"client", "target", "operation", "contract", "detail"} {
		if v.Fields[k] == "" {
			t.Errorf("contract_violation field %q is empty", k)
		}
	}

	// The return event records the verdict too, so a reader scanning only
	// returns still sees it.
	returns := eventsOfType(rt, "client_return")
	if got := returns[len(returns)-1].Fields["contract_ok"]; got != "false" {
		t.Errorf("client_return contract_ok = %q, want false", got)
	}
}

func TestClientCall_UndeclaredStatusIsAViolation(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "response"`)

	// The contract declares only 200 for getOrder; the server returns 503.
	resp := callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": "500"})
	if resp.Status != 503 {
		t.Fatalf("status = %d, want 503", resp.Status)
	}
	if resp.ContractOk {
		t.Error("an undeclared 503 should register as a contract violation")
	}
	if !strings.Contains(resp.ContractError, "503") {
		t.Errorf("contract_error = %q, want it to name the undeclared status", resp.ContractError)
	}

	// Non-2xx bodies are recorded on the return event, same rule as
	// step_recv, so a failure reads off the trace.
	returns := eventsOfType(rt, "client_return")
	if body := returns[len(returns)-1].Fields["body"]; !strings.Contains(body, "orders down") {
		t.Errorf("client_return body = %q, want the error payload", body)
	}
}

func TestClientCall_StrictRaisesOnViolation(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "strict"`)

	c, _ := rt.Client("mobile-app")
	op, _ := c.Table.Lookup("get_order")
	_, err := rt.executeClientCall(nil, c, op, map[string]any{"order_id": "degraded"})
	if err == nil {
		t.Fatal("validate=strict must raise on a response violation")
	}
	if !strings.Contains(err.Error(), "does not conform") {
		t.Errorf("error = %v, want a conformance complaint", err)
	}
	// The violation is still in the trace — raising must not cost evidence.
	if len(eventsOfType(rt, "contract_violation")) != 1 {
		t.Error("strict mode should still emit the contract_violation event")
	}
}

func TestClientCall_RequestValidationRejectsBadCall(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "request"`)

	c, _ := rt.Client("mobile-app")
	op, _ := c.Table.Lookup("create_order")
	// item_id is required by the requestBody schema.
	_, err := rt.executeClientCall(nil, c, op, map[string]any{
		"body": map[string]any{"qty": int64(2)},
	})
	if err == nil {
		t.Fatal("validate=request must reject a body that violates the schema")
	}
	if !strings.Contains(err.Error(), "request does not conform") {
		t.Errorf("error = %v, want a request-conformance complaint", err)
	}
	// Nothing went on the wire, so nothing should be in the trace.
	if n := len(eventsOfType(rt, "client_call")); n != 0 {
		t.Errorf("got %d client_call events for a rejected request, want 0", n)
	}
}

func TestClientCall_HeadersAndBeforeHook(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `,
    headers = {"X-Client": "ios/4.2"},
    before = lambda req: {"headers": {"Authorization": "Bearer tok"}}`)

	// POST /orders echoes back the X-Client header the server saw.
	resp := callOp(t, rt, "mobile-app", "create_order", map[string]any{
		"body": map[string]any{"item_id": "sku-1"},
	})
	if resp.Status != 201 {
		t.Fatalf("status = %d, want 201", resp.Status)
	}
	if !strings.Contains(resp.Body, "ios/4.2") {
		t.Errorf("server did not see the client-level header; body = %s", resp.Body)
	}

	// The before= hook's header reached the request. It isn't echoed, so
	// assert on what the client recorded instead.
	calls := eventsOfType(rt, "client_call")
	if len(calls) != 1 {
		t.Fatalf("got %d client_call events, want 1", len(calls))
	}
	if body := calls[0].Fields["body"]; !strings.Contains(body, "sku-1") {
		t.Errorf("client_call body = %q, want the encoded request", body)
	}
}

func TestClientCall_UnknownKwargIsRejectedAtCallTime(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, "")

	c, _ := rt.Client("mobile-app")
	op, _ := c.Table.Lookup("get_order")
	_, err := rt.executeClientCall(nil, c, op, map[string]any{"order_ids": int64(1)})
	if err == nil {
		t.Fatal("expected an unknown-argument error")
	}
	for _, want := range []string{`unknown argument "order_ids"`, `did you mean "order_id"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestClientEvents_ReachDownstreamConsumers covers the RFC-055 Phase 3
// migration: every consumer that had step_send/step_recv hard-coded must
// also recognise the client pair, or client calls go missing exactly where
// a reader looks for them.
func TestClientEvents_ReachDownstreamConsumers(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "response"`)

	callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": int64(42)})
	callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": "500"})

	t.Run("assertion context includes client calls", func(t *testing.T) {
		ctx := rt.recentAssertionContext(4)
		if len(ctx) == 0 {
			t.Fatal("recentAssertionContext returned nothing; client calls are invisible at failure time")
		}
		var sawCall, sawReturn bool
		for _, c := range ctx {
			switch c.Type {
			case "client_call":
				sawCall = true
			case "client_return":
				sawReturn = true
				if c.Target != "orders" {
					t.Errorf("context target = %q, want orders", c.Target)
				}
			}
		}
		if !sawCall || !sawReturn {
			t.Errorf("context = %+v, want both client_call and client_return", ctx)
		}
		// Chronological order, like the step path.
		for i := 1; i < len(ctx); i++ {
			if ctx[i].Seq < ctx[i-1].Seq {
				t.Errorf("context is not in chronological order: %+v", ctx)
				break
			}
		}
	})

	t.Run("normalized trace records operation not path", func(t *testing.T) {
		var lines []string
		for _, ev := range rt.events.Events() {
			switch ev.Type {
			case "client_call", "client_return":
				lines = append(lines, ev.Fields["operation"])
			}
		}
		if len(lines) != 4 {
			t.Fatalf("got %d client events, want 4", len(lines))
		}
		// Both calls normalize to the same operation even though their
		// paths differ (/orders/42 vs /orders/500) — that's the point of
		// keying the normalized line on the operation.
		for _, l := range lines {
			if l != "get_order" {
				t.Errorf("operation = %q, want get_order", l)
			}
		}
	})

	t.Run("isCallEventType covers both pairs", func(t *testing.T) {
		for _, typ := range []string{"step_send", "step_recv", "client_call", "client_return"} {
			if !isCallEventType(typ) {
				t.Errorf("isCallEventType(%q) = false, want true", typ)
			}
		}
		for _, typ := range []string{"syscall", "fault_applied", "contract_violation", ""} {
			if isCallEventType(typ) {
				t.Errorf("isCallEventType(%q) = true, want false", typ)
			}
		}
	})
}

// eventMatcher builds the same matcher shape match.event(type=…, **fields)
// produces, so these tests exercise the real matching path rather than a
// hand-rolled predicate.
func eventMatcher(typ string, fields map[string]string) *MatcherVal {
	criteria := map[string]string{"type": typ}
	for k, v := range fields {
		criteria[k] = v
	}
	return &MatcherVal{
		name:    "event(" + typ + ")",
		matchFn: func(ev Event) bool { return matchEventCriteria(ev, criteria) },
	}
}

// TestClientCall_ProxyFaultApplies is the regression guard for RFC-055's
// central composition claim: a client is the *same caller* as a step
// method as far as fault injection is concerned.
//
// It holds because clientAddr resolves through proxyMgr.GetProxyAddr
// exactly as executeStep does. Nothing else enforces that — refactor the
// address resolution and the whole "faults compose for free" story dies
// silently, with every client test still green. Hence this test.
func TestClientCall_ProxyFaultApplies(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, "")

	c, _ := rt.Client("mobile-app")
	// With no proxy up the client dials the interface directly, spelled
	// "localhost:<port>" — the same form executeStep builds.
	_, port, _ := net.SplitHostPort(addr)
	if got := rt.clientAddr(c); got != "localhost:"+port {
		t.Fatalf("clientAddr = %q, want localhost:%s with no proxy up", got, port)
	}

	// Stand in for a running proxy on the target interface. When one is
	// up, the client must dial *it* rather than the service directly —
	// that redirection is the entire fault-injection integration.
	iface := c.Target.Interface
	rt.proxyMgr.RegisterListenAddr(c.Target.Service.Name, iface.Name, "127.0.0.1:59999")

	if got := rt.clientAddr(c); got != "127.0.0.1:59999" {
		t.Errorf("clientAddr = %q with a proxy up, want the proxy listener — "+
			"client calls must traverse the fault path, not bypass it", got)
	}
}

// TestClientEvents_WorkAsTemporalAnchors is the regression guard for
// RFC-055 §5.6. Client events are ordinary events, so match.event() should
// select them with no new matcher syntax — that "no new syntax" property is
// what OQ-3 leaned on when it deferred the match.call() sugar, so it needs
// a test rather than an assurance.
func TestClientEvents_WorkAsTemporalAnchors(t *testing.T) {
	addr := startOrdersServer(t)
	rt := clientRuntime(t, addr, `, validate = "response"`)

	callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": int64(42)})
	callOp(t, rt, "mobile-app", "get_order", map[string]any{"order_id": "degraded"})

	cases := []struct {
		name    string
		matcher *MatcherVal
		want    int
	}{
		{
			name:    "by client and operation",
			matcher: eventMatcher("client_call", map[string]string{"client": "mobile-app", "operation": "get_order"}),
			want:    2,
		},
		{
			name:    "by a different client selects nothing",
			matcher: eventMatcher("client_call", map[string]string{"client": "partner-api"}),
			want:    0,
		},
		{
			name:    "by contract verdict",
			matcher: eventMatcher("client_return", map[string]string{"contract_ok": "false"}),
			want:    1,
		},
		{
			name:    "violations are matchable in their own right",
			matcher: eventMatcher("contract_violation", map[string]string{"client": "mobile-app"}),
			want:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(rt.events.MatchingEvents(tc.matcher))
			if got != tc.want {
				t.Errorf("matched %d events, want %d", got, tc.want)
			}
		})
	}

	// An anchor is only useful if it can open a window, which means
	// FirstMatching has to find it — that is the lookup eventually(anchor=)
	// and always(between=) both run.
	anchor := eventMatcher("client_return", map[string]string{"contract_ok": "false"})
	ev, found := rt.events.FirstMatching(anchor)
	if !found {
		t.Fatal("FirstMatching did not resolve the anchor; eventually(anchor=) would never open")
	}
	if ev.Fields["operation"] != "get_order" {
		t.Errorf("anchor resolved to operation %q, want get_order", ev.Fields["operation"])
	}
}

// TestClientGRPC_EndToEnd is the RFC-055 gRPC path exercised the way a
// user meets it: declared in a spec, called as a generated attribute, with
// the result read off the trace.
//
// Every other client test in this package is HTTP. The protocol-layer gRPC
// tests cover encode/invoke/decode, but nothing joined that to the Starlark
// surface — client(descriptors=) → attribute call → client event was
// untested as a whole, which is the shape of the RFC's own example.
//
// The mock and the client are built from the *same* descriptor set, so a
// pass means the typed encoder and the typed decoder agree on the wire.
func TestClientGRPC_EndToEnd(t *testing.T) {
	pbPath := writeTestDescriptorSet(t)
	port := freePortForTest(t)

	rt := New(testLogger())
	src := `
config = mock_service("config",
    interface("main", "grpc", ` + strconv.Itoa(port) + `),
    descriptors = "` + pbPath + `",
    routes = {
        "/test.config.ConfigService/GetSetting": grpc_typed_response(
            body = {"id": 42, "name": "primary", "scope": "default", "currency": "USD"},
        ),
    },
)

config_client = client("config-client", target = config.main, descriptors = "` + pbPath + `")
`
	if err := rt.LoadString("grpc_client.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	c, ok := rt.Client("config-client")
	if !ok {
		t.Fatal("config-client not registered")
	}

	// The single service in the set is selected without grpc_service=, and
	// GetSetting normalizes to get_setting.
	if names := c.Table.Names(); len(names) != 1 || names[0] != "get_setting" {
		t.Fatalf("operations = %v, want [get_setting]", names)
	}
	if got := c.Table.Contract.Version; got != "test.config.ConfigService" {
		t.Errorf("contract version = %q, want the service FQN", got)
	}

	// Start the mock the same way a test run would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	resp := callOp(t, rt, "config-client", "get_setting", map[string]any{"id": int64(42)})
	if !resp.Ok {
		t.Fatalf("call failed: %s", resp.Error)
	}

	// The response decoded as the real message type, not google.protobuf.Struct.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(resp.Body), &decoded); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, resp.Body)
	}
	if decoded["name"] != "primary" {
		t.Errorf("response name = %v, want primary (body %s)", decoded["name"], resp.Body)
	}
	if decoded["currency"] != "USD" {
		t.Errorf("response currency = %v, want USD", decoded["currency"])
	}
	if resp.Client != "config-client" || resp.Operation != "get_setting" {
		t.Errorf("provenance = client %q operation %q", resp.Client, resp.Operation)
	}

	// The trace records it on the client's own lane, with the gRPC status.
	calls := eventsOfType(rt, "client_call")
	returns := eventsOfType(rt, "client_return")
	if len(calls) != 1 || len(returns) != 1 {
		t.Fatalf("got %d client_call / %d client_return, want 1 each", len(calls), len(returns))
	}
	if calls[0].Service != "config-client" {
		t.Errorf("client_call service = %q, want config-client", calls[0].Service)
	}
	if got := calls[0].Fields["method_path"]; got != "/test.config.ConfigService/GetSetting" {
		t.Errorf("client_call method_path = %q", got)
	}
	if got := calls[0].EventType; got != "client_call.config" {
		t.Errorf("client_call event_type = %q, want client_call.config", got)
	}
	if got := returns[0].Fields["grpc_code"]; got != "OK" {
		t.Errorf("client_return grpc_code = %q, want OK", got)
	}
	if got := returns[0].Fields["success"]; got != "true" {
		t.Errorf("client_return success = %q, want true", got)
	}
}

// TestClientGRPC_UnroutedMethodSurfacesStatus checks the failure shape: a
// gRPC status is an outcome carried on the Response, not a Go error, so a
// test can assert on it the same way it would on an HTTP status.
func TestClientGRPC_UnroutedMethodSurfacesStatus(t *testing.T) {
	pbPath := writeTestDescriptorSet(t)
	port := freePortForTest(t)

	rt := New(testLogger())
	src := `
config = mock_service("config",
    interface("main", "grpc", ` + strconv.Itoa(port) + `),
    descriptors = "` + pbPath + `",
    routes = {},
)
config_client = client("config-client", target = config.main, descriptors = "` + pbPath + `")
`
	if err := rt.LoadString("grpc_client.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	resp := callOp(t, rt, "config-client", "get_setting", map[string]any{"id": int64(1)})
	if resp.Ok {
		t.Fatal("expected the unrouted method to fail")
	}
	if !strings.Contains(resp.Error, "Unimplemented") {
		t.Errorf("error = %q, want an Unimplemented status", resp.Error)
	}

	returns := eventsOfType(rt, "client_return")
	if len(returns) != 1 {
		t.Fatalf("got %d client_return events, want 1", len(returns))
	}
	if got := returns[0].Fields["grpc_code"]; got != "Unimplemented" {
		t.Errorf("client_return grpc_code = %q, want Unimplemented", got)
	}
	if got := returns[0].Fields["success"]; got != "false" {
		t.Errorf("client_return success = %q, want false", got)
	}
}

// freePortForTest reserves and releases a loopback port so a spec can name
// it before the service binds.
func freePortForTest(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
