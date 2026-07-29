package protocol

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// ordersSpec exercises the shapes a real document throws at the client
// builder: declared and missing operationIds, every parameter location,
// path-item-level shared parameters, a required request body, and multiple
// declared response codes with schemas.
const ordersSpec = `
openapi: 3.0.3
info:
  title: Orders
  version: 1.4.0
paths:
  /orders:
    get:
      operationId: listOrders
      parameters:
        - name: status
          in: query
          schema:
            type: string
        - name: tag
          in: query
          schema:
            type: array
            items:
              type: string
      responses:
        "200":
          description: order list
          content:
            application/json:
              schema:
                type: array
                items:
                  type: object
    post:
      operationId: createOrder
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [item_id, qty]
              properties:
                item_id:
                  type: string
                qty:
                  type: integer
      responses:
        "201":
          description: created
          content:
            application/json:
              schema:
                type: object
                properties:
                  id:
                    type: integer
  /orders/{orderId}:
    parameters:
      - name: X-Tenant
        in: header
        required: true
        schema:
          type: string
    get:
      operationId: getOrder
      parameters:
        - name: orderId
          in: path
          required: true
          schema:
            type: integer
        - name: include
          in: query
          schema:
            type: string
      responses:
        "200":
          description: one order
          content:
            application/json:
              schema:
                type: object
                required: [id, courier_eta]
                properties:
                  id:
                    type: integer
                  courier_eta:
                    type: string
        "404":
          description: missing
    delete:
      parameters:
        - name: orderId
          in: path
          required: true
          schema:
            type: integer
      responses:
        "204":
          description: gone
  /healthz:
    get:
      operationId: health
      responses:
        "200":
          description: ok
`

func loadOrdersTable(t *testing.T) *OperationTable {
	t.Helper()
	spec, err := LoadOpenAPI(writeSpec(t, ordersSpec))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	table, err := BuildOpenAPIOperations(spec, nil)
	if err != nil {
		t.Fatalf("BuildOpenAPIOperations: %v", err)
	}
	return table
}

func TestBuildOpenAPIOperations_NamesAndContract(t *testing.T) {
	table := loadOrdersTable(t)

	want := []string{"create_order", "delete_orders_order_id", "get_order", "health", "list_orders"}
	got := table.Names()
	if len(got) != len(want) {
		t.Fatalf("operation names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("operation names = %v, want %v", got, want)
			break
		}
	}

	if table.Contract.Kind != ContractOpenAPI {
		t.Errorf("contract kind = %q, want %q", table.Contract.Kind, ContractOpenAPI)
	}
	if table.Contract.Version != "1.4.0" {
		t.Errorf("contract version = %q, want 1.4.0", table.Contract.Version)
	}

	// DELETE /orders/{orderId} declares no operationId, so its name is
	// synthesized from the method and path — including the parameter's
	// name, not a positional placeholder.
	del, ok := table.Lookup("delete_orders_order_id")
	if !ok {
		t.Fatal("synthesized operation delete_orders_order_id not found")
	}
	if del.ContractName != "delete_orders_orderId" {
		t.Errorf("synthesized contract name = %q, want delete_orders_orderId", del.ContractName)
	}
	if del.Method != "DELETE" || del.PathTemplate != "/orders/{orderId}" {
		t.Errorf("synthesized op wire = %q, want DELETE /orders/{orderId}", del.Wire())
	}
}

func TestBuildOpenAPIOperations_ParamPartitioning(t *testing.T) {
	table := loadOrdersTable(t)
	op, ok := table.Lookup("get_order")
	if !ok {
		t.Fatal("get_order not found")
	}

	// Ordered path → query → header, then by name. The X-Tenant header is
	// declared on the path item, not the operation, and must still appear.
	want := []struct {
		name     string
		in       ParamLocation
		required bool
	}{
		{"order_id", ParamPath, true},
		{"include", ParamQuery, false},
		{"x_tenant", ParamHeader, true},
	}
	if len(op.Params) != len(want) {
		t.Fatalf("params = %+v, want %d entries", op.Params, len(want))
	}
	for i, w := range want {
		got := op.Params[i]
		if got.Name != w.name || got.In != w.in || got.Required != w.required {
			t.Errorf("param[%d] = {%s %s required=%v}, want {%s %s required=%v}",
				i, got.Name, got.In, got.Required, w.name, w.in, w.required)
		}
	}

	if op.AcceptsBody {
		t.Error("get_order should not accept a body")
	}

	create, _ := table.Lookup("create_order")
	if !create.AcceptsBody || !create.BodyRequired {
		t.Errorf("create_order: AcceptsBody=%v BodyRequired=%v, want both true",
			create.AcceptsBody, create.BodyRequired)
	}
	if hint := create.SignatureHint(); hint != "create_order(body)" {
		t.Errorf("SignatureHint() = %q, want create_order(body)", hint)
	}
}

func TestBuildHTTPRequest_BindsEveryLocation(t *testing.T) {
	table := loadOrdersTable(t)
	op, _ := table.Lookup("get_order")

	req, err := table.BuildHTTPRequest(op, map[string]any{
		"order_id": int64(42),
		"include":  "items",
		"x_tenant": "acme",
		"headers":  map[string]any{"X-Request-Id": "abc"},
	}, "", map[string]string{"X-Client": "ios/4.2"})
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}

	if req.Method != "GET" {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.Path != "/orders/42?include=items" {
		t.Errorf("path = %q, want /orders/42?include=items", req.Path)
	}
	for k, want := range map[string]string{
		"X-Tenant":     "acme",    // header parameter
		"X-Client":     "ios/4.2", // client-level default
		"X-Request-Id": "abc",     // per-call override
	} {
		if req.Headers[k] != want {
			t.Errorf("header %s = %q, want %q", k, req.Headers[k], want)
		}
	}
}

func TestBuildHTTPRequest_QueryListAndBasePath(t *testing.T) {
	table := loadOrdersTable(t)
	op, _ := table.Lookup("list_orders")

	req, err := table.BuildHTTPRequest(op, map[string]any{
		"status": "open",
		"tag":    []any{"urgent", "vip"},
	}, "/v1", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	if !strings.HasPrefix(req.Path, "/v1/orders?") {
		t.Errorf("path = %q, want the /v1 base path prefix", req.Path)
	}
	// url.Values.Encode sorts keys, so this ordering is stable.
	if req.Path != "/v1/orders?status=open&tag=urgent&tag=vip" {
		t.Errorf("path = %q, want repeated tag params", req.Path)
	}
}

func TestBuildHTTPRequest_BodyEncoding(t *testing.T) {
	table := loadOrdersTable(t)
	op, _ := table.Lookup("create_order")

	// Dict bodies are JSON-encoded.
	req, err := table.BuildHTTPRequest(op, map[string]any{
		"body": map[string]any{"item_id": "sku-1", "qty": int64(2)},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	if req.ContentType != "application/json" {
		t.Errorf("content type = %q, want application/json", req.ContentType)
	}
	if req.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type header = %q, want application/json", req.Headers["Content-Type"])
	}
	if got := string(req.Body); got != `{"item_id":"sku-1","qty":2}` {
		t.Errorf("body = %s, want the JSON-encoded dict", got)
	}

	// String bodies pass through verbatim — the hand-crafted-payload
	// escape hatch.
	req, err = table.BuildHTTPRequest(op, map[string]any{"body": `{"raw":true}`}, "", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest (string body): %v", err)
	}
	if got := string(req.Body); got != `{"raw":true}` {
		t.Errorf("body = %s, want verbatim passthrough", got)
	}
}

func TestBuildHTTPRequest_Errors(t *testing.T) {
	table := loadOrdersTable(t)
	getOrder, _ := table.Lookup("get_order")
	createOrder, _ := table.Lookup("create_order")
	health, _ := table.Lookup("health")

	cases := []struct {
		name string
		op   *Operation
		args map[string]any
		want []string
	}{
		{
			name: "missing required path param",
			op:   getOrder,
			args: map[string]any{"x_tenant": "acme"},
			want: []string{"missing required path parameter", "order_id"},
		},
		{
			name: "missing required header param",
			op:   getOrder,
			args: map[string]any{"order_id": int64(1)},
			want: []string{"missing required header parameter", "x_tenant"},
		},
		{
			name: "unknown kwarg suggests the nearest parameter",
			op:   getOrder,
			args: map[string]any{"order_ids": int64(1), "x_tenant": "acme"},
			want: []string{`unknown argument "order_ids"`, `did you mean "order_id"`, "declared parameters:"},
		},
		{
			name: "missing required body",
			op:   createOrder,
			args: map[string]any{},
			want: []string{"body= is required"},
		},
		{
			name: "body on an operation that declares none",
			op:   health,
			args: map[string]any{"body": "{}"},
			want: []string{"declares no request body"},
		},
		{
			name: "list value for a non-query parameter",
			op:   getOrder,
			args: map[string]any{"order_id": []any{1, 2}, "x_tenant": "acme"},
			want: []string{"cannot take a list"},
		},
		{
			name: "unsupported scalar type",
			op:   getOrder,
			args: map[string]any{"order_id": map[string]any{}, "x_tenant": "acme"},
			want: []string{"unsupported parameter value"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := table.BuildHTTPRequest(c.op, c.args, "", nil)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err.Error(), w)
				}
			}
		})
	}
}

func TestBuildOpenAPIOperations_CollisionAndRename(t *testing.T) {
	const colliding = `
openapi: 3.0.3
info: {title: X, version: "1"}
paths:
  /a:
    get:
      operationId: getOrder
      responses: {"200": {description: ok}}
  /b:
    get:
      operationId: get-order
      responses: {"200": {description: ok}}
`
	spec, err := LoadOpenAPI(writeSpec(t, colliding))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}

	_, err = BuildOpenAPIOperations(spec, nil)
	if err == nil {
		t.Fatal("expected a collision error")
	}
	if !strings.Contains(err.Error(), "collision") || !strings.Contains(err.Error(), "rename=") {
		t.Errorf("collision error should name the fix, got: %v", err)
	}

	// The documented fix resolves it.
	table, err := BuildOpenAPIOperations(spec, map[string]string{"get-order": "get_order_b"})
	if err != nil {
		t.Fatalf("BuildOpenAPIOperations with rename: %v", err)
	}
	if _, ok := table.Lookup("get_order_b"); !ok {
		t.Errorf("renamed operation not found; names = %v", table.Names())
	}
	if _, ok := table.Lookup("get_order"); !ok {
		t.Errorf("original operation missing; names = %v", table.Names())
	}
}

func TestBuildOpenAPIOperations_UnusedRenameIsAnError(t *testing.T) {
	spec, err := LoadOpenAPI(writeSpec(t, ordersSpec))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	_, err = BuildOpenAPIOperations(spec, map[string]string{"noSuchOp": "whatever"})
	if err == nil {
		t.Fatal("expected an error for a rename key matching no operation")
	}
	if !strings.Contains(err.Error(), "noSuchOp") {
		t.Errorf("error should name the unused key, got: %v", err)
	}
}

func TestBuildOpenAPIOperations_ReservedParamName(t *testing.T) {
	const reserved = `
openapi: 3.0.3
info: {title: X, version: "1"}
paths:
  /search:
    get:
      operationId: search
      parameters:
        - name: headers
          in: query
          schema: {type: string}
      responses: {"200": {description: ok}}
`
	spec, err := LoadOpenAPI(writeSpec(t, reserved))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	_, err = BuildOpenAPIOperations(spec, nil)
	if err == nil {
		t.Fatal("expected an error for a parameter colliding with a reserved kwarg")
	}
	for _, want := range []string{"reserved kwarg", `"headers"`, "client.call("} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestBuildOpenAPIOperations_NoPaths(t *testing.T) {
	const empty = `
openapi: 3.0.3
info: {title: X, version: "1"}
paths: {}
`
	spec, err := LoadOpenAPI(writeSpec(t, empty))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	if _, err := BuildOpenAPIOperations(spec, nil); err == nil {
		t.Fatal("expected an error for a document with no paths")
	}
}

func TestValidateHTTPResponse(t *testing.T) {
	table := loadOrdersTable(t)
	getOrder, _ := table.Lookup("get_order")
	del, _ := table.Lookup("delete_orders_order_id")

	t.Run("conforming response passes", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 200, "application/json",
			[]byte(`{"id": 1, "courier_eta": "12m"}`))
		if err != nil {
			t.Errorf("expected conformance, got: %v", err)
		}
	})

	t.Run("null in a required field is a violation", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 200, "application/json",
			[]byte(`{"id": 1, "courier_eta": null}`))
		if err == nil {
			t.Fatal("expected a schema violation for a null courier_eta")
		}
	})

	t.Run("missing required field is a violation", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 200, "application/json", []byte(`{"id": 1}`))
		if err == nil {
			t.Fatal("expected a schema violation for the missing courier_eta")
		}
	})

	t.Run("undeclared status is a violation", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 503, "application/json", []byte(`{"error":"down"}`))
		if err == nil {
			t.Fatal("expected a violation for an undeclared 503")
		}
		if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "declared:") {
			t.Errorf("error should name the status and what was declared, got: %v", err)
		}
	})

	t.Run("declared status with no content accepts anything", func(t *testing.T) {
		if err := table.ValidateHTTPResponse(getOrder, 404, "application/json", []byte(`{"x":1}`)); err != nil {
			t.Errorf("404 declares no content, so any body conforms; got: %v", err)
		}
		if err := table.ValidateHTTPResponse(del, 204, "", nil); err != nil {
			t.Errorf("204 status-only should conform; got: %v", err)
		}
	})

	t.Run("malformed JSON is a violation", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 200, "application/json", []byte(`{"id":`))
		if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
			t.Errorf("expected a malformed-JSON violation, got: %v", err)
		}
	})

	t.Run("undeclared content type is a violation", func(t *testing.T) {
		err := table.ValidateHTTPResponse(getOrder, 200, "text/html", []byte(`<html>`))
		if err == nil || !strings.Contains(err.Error(), "content type") {
			t.Errorf("expected a content-type violation, got: %v", err)
		}
	})
}

func TestValidateHTTPRequest(t *testing.T) {
	table := loadOrdersTable(t)
	op, _ := table.Lookup("create_order")

	good, err := table.BuildHTTPRequest(op, map[string]any{
		"body": map[string]any{"item_id": "sku-1", "qty": int64(2)},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	if err := table.ValidateHTTPRequest(op, good); err != nil {
		t.Errorf("expected a conforming request, got: %v", err)
	}

	// qty is declared as an integer; a string violates the schema.
	bad, err := table.BuildHTTPRequest(op, map[string]any{
		"body": map[string]any{"item_id": "sku-1", "qty": "two"},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	if err := table.ValidateHTTPRequest(op, bad); err == nil {
		t.Error("expected a schema violation for a string qty")
	}

	// Missing a required property is likewise caught before the wire.
	missing, err := table.BuildHTTPRequest(op, map[string]any{
		"body": map[string]any{"item_id": "sku-1"},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	if err := table.ValidateHTTPRequest(op, missing); err == nil {
		t.Error("expected a schema violation for the missing qty")
	}
}

// TestExecuteHTTPRequest_ContractLoop drives an RFC-021 mock generated from
// the *same* OpenAPI document the client was built from. This is the loop
// the feature exists to make checkable: contract → mock (callee), contract →
// client (caller), and a conformance verdict on what came back.
func TestExecuteHTTPRequest_ContractLoop(t *testing.T) {
	specPath := writeSpec(t, ordersSpec)
	spec, err := LoadOpenAPI(specPath)
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	table, err := BuildOpenAPIOperations(spec, nil)
	if err != nil {
		t.Fatalf("BuildOpenAPIOperations: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// A conforming route for get_order, plus a deliberately non-conforming
	// one that returns a null in a required field — the degraded-response
	// shape validate="response" exists to catch.
	routes := []MockRoute{
		{
			Pattern: "GET /orders/*",
			Response: &MockResponse{
				Status:      200,
				Body:        []byte(`{"id": 1001, "courier_eta": "12m"}`),
				ContentType: "application/json",
			},
		},
		{
			Pattern: "GET /healthz",
			Response: &MockResponse{
				Status:      200,
				Body:        []byte(`{"courier_eta": null}`),
				ContentType: "application/json",
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, ok := Get("http")
	if !ok {
		t.Fatal("http protocol not registered")
	}
	server, ok := p.(MockHandler)
	if !ok {
		t.Fatal("http protocol does not implement MockHandler")
	}
	go func() { _ = server.ServeMock(ctx, addr, MockSpec{Routes: routes}, nil) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if derr == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Run("conforming response", func(t *testing.T) {
		op, _ := table.Lookup("get_order")
		req, err := table.BuildHTTPRequest(op, map[string]any{
			"order_id": int64(1001),
			"x_tenant": "acme",
		}, "", nil)
		if err != nil {
			t.Fatalf("BuildHTTPRequest: %v", err)
		}
		res, err := ExecuteHTTPRequest(context.Background(), addr, req)
		if err != nil {
			t.Fatalf("ExecuteHTTPRequest: %v", err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("status = %d, want 200 (body %s)", res.StatusCode, res.Body)
		}
		if ct := ResponseContentType(res); !strings.Contains(ct, "json") {
			t.Errorf("content type = %q, want a JSON media type", ct)
		}
		if err := table.ValidateHTTPResponse(op, res.StatusCode, ResponseContentType(res), []byte(res.Body)); err != nil {
			t.Errorf("expected a conforming response, got: %v", err)
		}
	})

	t.Run("null in a required field is caught", func(t *testing.T) {
		op, _ := table.Lookup("get_order")
		// Serve the bad payload through the /healthz route, then validate
		// it against get_order's schema — the same check the client runs
		// when a service degrades.
		health, _ := table.Lookup("health")
		req, err := table.BuildHTTPRequest(health, nil, "", nil)
		if err != nil {
			t.Fatalf("BuildHTTPRequest: %v", err)
		}
		res, err := ExecuteHTTPRequest(context.Background(), addr, req)
		if err != nil {
			t.Fatalf("ExecuteHTTPRequest: %v", err)
		}
		err = table.ValidateHTTPResponse(op, res.StatusCode, ResponseContentType(res), []byte(res.Body))
		if err == nil {
			t.Fatal("expected a contract violation for the null courier_eta")
		}
	})
}

func TestJoinBasePath(t *testing.T) {
	cases := []struct{ base, p, want string }{
		{"", "/orders", "/orders"},
		{"/", "/orders", "/orders"},
		{"/v1", "/orders", "/v1/orders"},
		{"v1", "/orders", "/v1/orders"},
		{"/v1/", "/orders", "/v1/orders"},
		{"/v1", "/orders/", "/v1/orders/"},
	}
	for _, c := range cases {
		if got := joinBasePath(c.base, c.p); got != c.want {
			t.Errorf("joinBasePath(%q, %q) = %q, want %q", c.base, c.p, got, c.want)
		}
	}
}
