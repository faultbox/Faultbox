package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPMockStaticRoutes verifies that the HTTP MockHandler matches
// METHOD PATTERN routes, returns the configured status/body/headers, and
// falls back to 404 when no route matches.
func TestHTTPMockStaticRoutes(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "GET /.well-known/openid-configuration/jwks",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"keys":[{"kty":"OKP"}]}`),
					ContentType: "application/json",
				},
			},
			{
				Pattern: "GET /health",
				Response: &MockResponse{Status: 204},
			},
			{
				Pattern: "POST /api/v1/**",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"ok":true}`),
					ContentType: "application/json",
					Headers:     map[string]string{"X-Mock": "1"},
				},
			},
		},
	}

	emitted := &recordedEmits{}
	serveErr := make(chan error, 1)
	go func() { serveErr <- p.ServeMock(ctx, addr, spec, emitted.emit) }()

	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}

	// Route 1: JWKS endpoint.
	resp, err := client.Get(fmt.Sprintf("http://%s/.well-known/openid-configuration/jwks", addr))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("jwks status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"kty":"OKP"`) {
		t.Fatalf("jwks body = %q", string(body))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("jwks content-type = %q", resp.Header.Get("Content-Type"))
	}

	// Route 2: status-only.
	resp, err = client.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("health status = %d, want 204", resp.StatusCode)
	}

	// Route 3: glob ** match + custom header.
	resp, err = client.Post(fmt.Sprintf("http://%s/api/v1/users/42", addr), "application/json", strings.NewReader(`{"id":42}`))
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Mock") != "1" {
		t.Fatalf("api X-Mock = %q", resp.Header.Get("X-Mock"))
	}

	// Unmatched → 404.
	resp, err = client.Get(fmt.Sprintf("http://%s/does-not-exist", addr))
	if err != nil {
		t.Fatalf("GET unmatched: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unmatched status = %d, want 404", resp.StatusCode)
	}

	if got := emitted.count(); got != 4 {
		t.Fatalf("emitter called %d times, want 4", got)
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTPMockDynamicHandler verifies that a dynamic handler receives
// request context and that its returned MockResponse is written back.
func TestHTTPMockDynamicHandler(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	var observed MockRequest
	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "POST /token",
				Dynamic: func(req MockRequest) (*MockResponse, error) {
					observed = req
					body, _ := json.Marshal(map[string]string{
						"user": req.Query["user"],
						"body": string(req.Body),
					})
					return &MockResponse{
						Status:      200,
						Body:        body,
						ContentType: "application/json",
					}, nil
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://%s/token?user=alice", addr),
		"application/json",
		strings.NewReader(`{"hello":"world"}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if observed.Method != "POST" {
		t.Fatalf("observed.Method = %q", observed.Method)
	}
	if observed.Query["user"] != "alice" {
		t.Fatalf("observed.Query[user] = %q", observed.Query["user"])
	}
	if string(observed.Body) != `{"hello":"world"}` {
		t.Fatalf("observed.Body = %q", string(observed.Body))
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"user":"alice"`) {
		t.Fatalf("response body = %q", string(body))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTPMockDefaultResponse verifies that unmatched requests fall through
// to the configured default when set.
func TestHTTPMockDefaultResponse(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Default: &MockResponse{
			Status:      501,
			Body:        []byte(`unsupported`),
			ContentType: "text/plain",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/anything", addr))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// TestMatchGlob covers the path glob patterns directly.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		target  string
		want    bool
	}{
		{"/health", "/health", true},
		{"/health", "/health/v2", false},
		{"/api/*", "/api/users", true},
		{"/api/*", "/api/users/42", false},
		{"/api/**", "/api/users/42/posts", true},
		{"/api/**/posts", "/api/users/42/posts", true},
		{"/api/**/posts", "/api/42/users/posts", true},
		{"/api/**/posts", "/api/42/users", false},
		{"/.well-known/openid-configuration/jwks", "/.well-known/openid-configuration/jwks", true},
	}
	for _, tc := range cases {
		got := matchGlob(tc.pattern, tc.target)
		if got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
		}
	}
}

// TestHTTP2MockStaticRoutes verifies the HTTP/2 MockHandler over h2c —
// same route table as HTTP, served over cleartext HTTP/2. Clients connect
// with an h2c-capable transport to confirm the response protocol is HTTP/2.
func TestHTTP2MockStaticRoutes(t *testing.T) {
	p := &http2Protocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "GET /healthz",
				Response: &MockResponse{Status: 200, Body: []byte(`ok`), ContentType: "text/plain"},
			},
			{
				Pattern: "POST /api/v1/**",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"ok":true}`),
					ContentType: "application/json",
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := newH2CClient(2 * time.Second)

	// Route 1 — verify protocol is HTTP/2.0.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("healthz proto = %q, want HTTP/2.0", resp.Proto)
	}
	if string(body) != "ok" {
		t.Fatalf("healthz body = %q", string(body))
	}

	// Route 2 — glob ** match.
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/api/v1/orders/42", strings.NewReader(`{"x":1}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("api proto = %q, want HTTP/2.0", resp.Proto)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("api body = %q", string(body))
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTP2MockDynamicHandler verifies dynamic handlers work over h2c too.
func TestHTTP2MockDynamicHandler(t *testing.T) {
	p := &http2Protocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{{
			Pattern: "POST /echo",
			Dynamic: func(req MockRequest) (*MockResponse, error) {
				payload, _ := json.Marshal(map[string]string{
					"method": req.Method,
					"path":   req.Path,
					"body":   string(req.Body),
				})
				return &MockResponse{Status: 200, Body: payload, ContentType: "application/json"}, nil
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := newH2CClient(2 * time.Second)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/echo", strings.NewReader(`{"ping":"pong"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST echo: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("echo proto = %q, want HTTP/2.0", resp.Proto)
	}
	if !strings.Contains(string(body), `"body":"{\"ping\":\"pong\"}"`) {
		t.Fatalf("echo body = %q", string(body))
	}
}

// --- test helpers ---

type recordedEmits struct {
	mu    sync.Mutex
	count_ int64
}

func (r *recordedEmits) emit(op string, fields map[string]string) {
	atomic.AddInt64(&r.count_, 1)
}

func (r *recordedEmits) count() int64 {
	return atomic.LoadInt64(&r.count_)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitListening(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("listener on %s not ready after %s", addr, timeout)
}
