package star

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
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

// TestMockServiceHTTP2 verifies a mock_service declared with protocol http2
// stands up an h2c listener and serves the same route table format.
func TestMockServiceHTTP2(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
gw = mock_service("gw",
    interface("public", "http2", %d),
    routes = {
        "GET /healthz":    status_only(200),
        "POST /api/v1/**": json_response(status = 200, body = {"ok": True}),
    },
)

def test_h2_stub():
    pass
`, port)
	if err := rt.LoadString("h2_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	client := newTestH2CClient()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("healthz proto = %q, want HTTP/2.0", resp.Proto)
	}

	req, _ = http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/api/v1/orders/42", strings.NewReader(`{"x":1}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("api body = %q", string(body))
	}
}

// TestMockServiceTCP verifies a TCP mock_service stands up and responds to
// line-framed input according to bytes_response() routes.
func TestMockServiceTCP(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
legacy = mock_service("legacy",
    interface("main", "tcp", %d),
    routes = {
        "PING\n":    bytes_response(data = "PONG\n"),
        "VERSION\n": bytes_response(data = "2.0.0\n"),
    },
    default = bytes_response(data = "ERR\n"),
)

def test_tcp():
    pass
`, port)
	if err := rt.LoadString("tcp_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cases := []struct{ in, want string }{
		{"PING\n", "PONG\n"},
		{"VERSION\n", "2.0.0\n"},
		{"HUH\n", "ERR\n"},
	}
	for _, tc := range cases {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_, _ = conn.Write([]byte(tc.in))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Close()
		if string(buf[:n]) != tc.want {
			t.Errorf("send %q: got %q, want %q", tc.in, buf[:n], tc.want)
		}
	}
}

// TestMockServiceUDP verifies a UDP mock_service swallows datagrams by
// default and emits one event per datagram into the runtime event log.
func TestMockServiceUDP(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
statsd = mock_service("statsd",
    interface("main", "udp", %d),
)
`, port)
	if err := rt.LoadString("udp_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for i := 0; i < 5; i++ {
		_, _ = conn.Write([]byte(fmt.Sprintf("gauge.%d:%d|g", i, i)))
	}
	conn.Close()

	// Event log should have >=5 mock.recv entries after a brief wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, e := range rt.events.Events() {
			if e.Type == "mock.recv" {
				count++
			}
		}
		if count >= 5 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected >=5 mock.recv events; final events: %+v", rt.events.Events())
}

// newTestH2CClient returns an HTTP client that speaks h2c. Local to this
// file to avoid reaching into the protocol package's unexported helpers.
func newTestH2CClient() *http.Client {
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
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
