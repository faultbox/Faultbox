package star

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// starString reads a string global out of a loaded spec.
func starString(t *testing.T, rt *Runtime, name string) string {
	t.Helper()
	v, ok := rt.globals[name]
	if !ok {
		t.Fatalf("global %q not found", name)
	}
	s, ok := starlark.AsString(v)
	if !ok {
		t.Fatalf("global %q is %s, not a string", name, v.Type())
	}
	return s
}

// starList reads a list-of-strings global out of a loaded spec.
func starList(t *testing.T, rt *Runtime, name string) []string {
	t.Helper()
	v, ok := rt.globals[name]
	if !ok {
		t.Fatalf("global %q not found", name)
	}
	list, ok := v.(*starlark.List)
	if !ok {
		t.Fatalf("global %q is %s, not a list", name, v.Type())
	}
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			t.Fatalf("global %q[%d] is %s, not a string", name, i, list.Index(i).Type())
		}
		out = append(out, s)
	}
	return out
}

// RFC-055 Phase 2 — the Starlark surface of client(). Spec-load behaviour
// (contract resolution, name validation, generated attributes) is covered
// here; the trace-emission half is exercised in client_trace_test.go.

const clientOrdersSpec = `
openapi: 3.0.3
info:
  title: Orders
  version: 2.1.0
paths:
  /orders/{orderId}:
    get:
      operationId: getOrder
      parameters:
        - name: orderId
          in: path
          required: true
          schema: {type: integer}
      responses:
        "200":
          description: one order
          content:
            application/json:
              schema:
                type: object
                required: [id, courier_eta]
                properties:
                  id: {type: integer}
                  courier_eta: {type: string}
  /orders:
    post:
      operationId: createOrder
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [item_id]
              properties:
                item_id: {type: string}
                qty: {type: integer}
      responses:
        "201":
          description: created
          content:
            application/json:
              schema: {type: object}
`

// writeSpecFile drops a contract next to a spec directory and returns the
// directory, so LoadFile-relative resolution can be exercised.
func writeSpecFile(t *testing.T, name, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return dir, path
}

func loadClientSpec(t *testing.T, src string) *Runtime {
	t.Helper()
	rt := New(testLogger())
	if err := rt.LoadString("client_test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	return rt
}

func TestClient_GeneratesOperationsFromOpenAPI(t *testing.T) {
	_, specPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	rt := loadClientSpec(t, `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "`+specPath+`")

OPS      = api.operations
NAME     = api.name
CONTRACT = api.contract
SIG      = api.get_order.signature
WIRE     = api.get_order.wire
`)

	if got := starString(t, rt, "NAME"); got != "mobile-app" {
		t.Errorf("api.name = %q, want mobile-app", got)
	}
	if got := starString(t, rt, "CONTRACT"); !strings.HasSuffix(got, "orders.yaml@2.1.0") {
		t.Errorf("api.contract = %q, want it to carry the document version", got)
	}
	if got := starString(t, rt, "SIG"); got != "get_order(order_id)" {
		t.Errorf("signature = %q, want get_order(order_id)", got)
	}
	if got := starString(t, rt, "WIRE"); got != "GET /orders/{orderId}" {
		t.Errorf("wire = %q, want GET /orders/{orderId}", got)
	}

	ops := starList(t, rt, "OPS")
	want := []string{"create_order", "get_order"}
	if len(ops) != len(want) || ops[0] != want[0] || ops[1] != want[1] {
		t.Errorf("operations = %v, want %v", ops, want)
	}

	c, ok := rt.Client("mobile-app")
	if !ok {
		t.Fatal("client not registered on the runtime")
	}
	if c.Target.Service.Name != "orders" {
		t.Errorf("target service = %q, want orders", c.Target.Service.Name)
	}
	if names := rt.ClientNames(); len(names) != 1 || names[0] != "mobile-app" {
		t.Errorf("ClientNames() = %v, want [mobile-app]", names)
	}
}

func TestClient_InheritsContractFromInterfaceSpec(t *testing.T) {
	_, specPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	rt := loadClientSpec(t, `
orders = service("orders",
    interface("public", "http", 8080, spec = "`+specPath+`"), image = "orders:1")
api = client("mobile-app", target = orders.public)
OPS = api.operations
`)
	ops := starList(t, rt, "OPS")
	if len(ops) != 2 {
		t.Errorf("operations = %v, want the two from the inherited contract", ops)
	}
}

func TestClient_UnknownOperationSuggests(t *testing.T) {
	_, specPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	rt := New(testLogger())
	err := rt.LoadString("client_test.star", `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "`+specPath+`")
X = api.get_orders
`)
	if err == nil {
		t.Fatal("expected an error for an unknown operation")
	}
	for _, want := range []string{`no operation "get_orders"`, `did you mean "get_order"`, "faultbox inspect --clients"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestClient_LoadErrors(t *testing.T) {
	_, openapiPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "no contract at all",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public)`,
			want: []string{"no contract", "openapi=", "interface("},
		},
		{
			name: "both contracts supplied",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `", descriptors = "x.pb")`,
			want: []string{"either openapi= or descriptors=, not both"},
		},
		{
			name: "target is not an interface",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders, openapi = "` + openapiPath + `")`,
			want: []string{"target= must be a service interface"},
		},
		{
			name: "client name collides with a service",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("orders", target = orders.public, openapi = "` + openapiPath + `")`,
			want: []string{"a service is already named", "vector-clock namespace"},
		},
		{
			name: "service name collides with a client",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("billing", target = orders.public, openapi = "` + openapiPath + `")
dup = service("billing", interface("public", "http", 8081), image = "billing:1")`,
			want: []string{"a client is already named", "vector-clock namespace"},
		},
		{
			name: "client named test",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("test", target = orders.public, openapi = "` + openapiPath + `")`,
			want: []string{"test driver's own trace identity"},
		},
		{
			name: "duplicate client name",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
a = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `")
b = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `")`,
			want: []string{"already declared"},
		},
		{
			name: "bad validate mode",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `", validate = "loose")`,
			want: []string{"validate=", "off, request, response, strict"},
		},
		{
			name: "bad timeout",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `", timeout = "soon")`,
			want: []string{"timeout="},
		},
		{
			name: "grpc_service with openapi",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `", grpc_service = "pkg.Svc")`,
			want: []string{"grpc_service= applies to descriptors="},
		},
		{
			name: "before is not callable",
			src: `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + openapiPath + `", before = 42)`,
			want: []string{"before= must be callable"},
		},
		{
			name: "unknown contract extension on interface spec",
			src: `
orders = service("orders",
    interface("public", "http", 8080, spec = "./contract.thrift"), image = "orders:1")
api = client("mobile-app", target = orders.public)`,
			want: []string{"cannot tell what kind of contract"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := New(testLogger())
			err := rt.LoadString("client_test.star", c.src)
			if err == nil {
				t.Fatal("expected a load error, got nil")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err.Error(), w)
				}
			}
		})
	}
}

func TestClient_ResolvesContractRelativeToSpecDir(t *testing.T) {
	dir, _ := writeSpecFile(t, "orders.yaml", clientOrdersSpec)
	specPath := filepath.Join(dir, "faultbox.star")
	if err := os.WriteFile(specPath, []byte(`
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "./orders.yaml")
OPS = api.operations
`), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	rt := New(testLogger())
	if err := rt.LoadFile(specPath); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if ops := starList(t, rt, "OPS"); len(ops) != 2 {
		t.Errorf("operations = %v, want 2 — the relative contract path should resolve against the spec dir", ops)
	}
}

func TestClient_OperationShadowingReservedAttrIsRejected(t *testing.T) {
	const shadowing = `
openapi: 3.0.3
info: {title: X, version: "1"}
paths:
  /call:
    get:
      operationId: call
      responses: {"200": {description: ok}}
`
	_, specPath := writeSpecFile(t, "shadow.yaml", shadowing)

	rt := New(testLogger())
	err := rt.LoadString("client_test.star", `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "`+specPath+`")
`)
	if err == nil {
		t.Fatal("expected an error for an operation shadowing a client attribute")
	}
	for _, want := range []string{"shadows the built-in client attribute", "rename="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestClient_PositionalArgsRejected(t *testing.T) {
	_, specPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	rt := New(testLogger())
	err := rt.LoadString("client_test.star", `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "`+specPath+`")

def test_positional(t):
    api.get_order(42)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	// The rejection happens at call time; assert the message shape via a
	// direct invocation rather than running the test body.
	c, _ := rt.Client("mobile-app")
	op, ok := c.Table.Lookup("get_order")
	if !ok {
		t.Fatal("get_order missing")
	}
	ov := &OperationVal{client: c, op: op}
	_, callErr := ov.CallInternal(nil, starlark.Tuple{starlark.MakeInt(42)}, nil)
	if callErr == nil {
		t.Fatal("expected positional arguments to be rejected")
	}
	if !strings.Contains(callErr.Error(), "keyword arguments only") {
		t.Errorf("error = %v, want a keyword-only complaint", callErr)
	}
}

// TestClient_IsNotAFaultTarget checks the diagnostic for a mistake the
// entity invites: a client looks like a service in a spec, so reaching for
// fault(client, ...) is a natural first guess. It's wrong — a client runs
// no code and has no syscalls — and the error has to say what to do
// instead, not just name the type it rejected (RFC-055 §5.7).
func TestClient_IsNotAFaultTarget(t *testing.T) {
	_, specPath := writeSpecFile(t, "orders.yaml", clientOrdersSpec)

	preamble := `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "` + specPath + `")
`
	cases := []struct {
		name string
		call string
		want []string
	}{
		{
			name: "fault()",
			call: `fault(api, write = deny("EIO"), run = lambda: None)`,
			// Must name the client, explain why, and show both the
			// protocol-level and syscall-level forms against the target.
			want: []string{
				"fault(mobile-app)",
				"a client is a caller, not a process",
				"fault(orders.public, response(status = 503)",
				`fault(orders, write = deny("EIO")`,
			},
		},
		{
			name: "fault_start()",
			call: `fault_start(api, write = deny("EIO"))`,
			want: []string{
				"fault_start(mobile-app)",
				"a client is a caller, not a process",
				"fault_start(orders,",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := New(testLogger())
			err := rt.LoadString("client_test.star", preamble+"\n"+c.call+"\n")
			if err == nil {
				t.Fatal("expected an error; a client is not a fault target")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%s", w, err.Error())
				}
			}
		})
	}
}
