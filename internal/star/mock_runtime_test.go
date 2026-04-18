package star

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMockServiceSpecLoad verifies mock_service() parses and registers a
// service with correct routes + flags, without starting the runtime.
func TestMockServiceSpecLoad(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
auth = mock_service("auth",
    interface("http", "http", 18090),
    routes = {
        "GET /.well-known/jwks": json_response(status = 200, body = {"keys": []}),
        "GET /health":            status_only(204),
    },
    default = json_response(status = 404, body = {"error": "not found"}),
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	services := rt.Services()
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	svc := services[0]
	if !svc.IsMock() {
		t.Fatalf("expected service to be mock")
	}
	if svc.IsContainer() {
		t.Fatalf("mock must not report IsContainer")
	}
	if _, ok := svc.Interfaces["http"]; !ok {
		t.Fatalf("missing http interface: %v", svc.Interfaces)
	}
	routes := svc.Mock.Routes["http"]
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].Pattern != "GET /.well-known/jwks" {
		t.Fatalf("routes[0].Pattern = %q", routes[0].Pattern)
	}
	if svc.Mock.Default["http"] == nil {
		t.Fatal("default response missing")
	}
}

// TestMockServiceUnknownProtocol verifies the spec-load guard on protocols
// without a MockHandler implementation (e.g., postgres today).
func TestMockServiceUnknownProtocol(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = mock_service("db",
    interface("main", "postgres", 5432),
)
`)
	if err == nil {
		t.Fatal("expected spec load to fail for protocol without MockHandler")
	}
	if !strings.Contains(err.Error(), "does not support mock_service") {
		t.Fatalf("error mismatch: %v", err)
	}
}

// TestMockServiceEndToEnd stands up a mock HTTP service via the runtime,
// issues a real HTTP request against it, and verifies the response.
func TestMockServiceEndToEnd(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
auth = mock_service("auth",
    interface("http", "http", %d),
    routes = {
        "GET /.well-known/jwks": json_response(status = 200, body = {"keys": [{"kid": "test-1"}]}),
        "POST /token":           dynamic(lambda req: json_response(status = 200, body = {"query_user": req["query"].get("user", "anon")})),
    },
)

def test_auth_stub():
    pass
`, port)
	if err := rt.LoadString("mock_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}

	// Static JSON route.
	resp, err := client.Get("http://" + addr + "/.well-known/jwks")
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("jwks status = %d, want 200, body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"kid":"test-1"`) {
		t.Fatalf("jwks body = %q", string(body))
	}

	// Dynamic route (lambda in spec inspects request).
	resp, err = client.Post("http://"+addr+"/token?user=alice", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("token status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"query_user":"alice"`) {
		t.Fatalf("token body = %q", string(body))
	}

	// Unmatched → 404 (default fallback).
	resp, err = client.Get("http://" + addr + "/missing")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing status = %d, want 404", resp.StatusCode)
	}

	// Event log should have mock.<op> entries for every handled request.
	events := rt.events.Events()
	mockEvents := 0
	for _, e := range events {
		if strings.HasPrefix(e.Type, "mock.") {
			mockEvents++
		}
	}
	if mockEvents < 3 {
		t.Fatalf("expected >=3 mock.<op> events, got %d (events=%+v)", mockEvents, events)
	}
}

// TestMockServiceRestartClean verifies that startServices → stopServices
// frees the port so a subsequent test can bind it again. Protects against
// goroutine leaks from the mock handler.
func TestMockServiceRestartClean(t *testing.T) {
	port := freePort(t)

	for i := 0; i < 2; i++ {
		rt := New(testLogger())
		src := fmt.Sprintf(`
s = mock_service("s", interface("http", "http", %d),
    routes = {"GET /": status_only(200)})
`, port)
		if err := rt.LoadString("restart.star", src); err != nil {
			t.Fatalf("iter %d LoadString: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rt.startServices(ctx); err != nil {
			cancel()
			t.Fatalf("iter %d start: %v", i, err)
		}
		rt.stopServices()
		cancel()

		// Port should be re-bindable immediately.
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("iter %d rebind: %v", i, err)
		}
		ln.Close()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
