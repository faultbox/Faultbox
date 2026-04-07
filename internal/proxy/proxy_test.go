package proxy

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

func TestHTTPProxyForward(t *testing.T) {
	// Start a mock HTTP server.
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok from real server")
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	// Start proxy pointing at mock.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTPProxy(nil, "test-svc")
	proxyAddr, err := p.Start(ctx, mockLn.Addr().String())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	// Request through proxy — should forward.
	resp, err := http.Get("http://" + proxyAddr + "/hello")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok from real server" {
		t.Errorf("body = %q", string(body))
	}
}

func TestHTTPProxyErrorRule(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events []ProxyEvent
	p := newHTTPProxy(func(evt ProxyEvent) {
		events = append(events, evt)
	}, "test-svc")
	proxyAddr, _ := p.Start(ctx, mockLn.Addr().String())
	defer p.Stop()

	// Add rule: POST /orders → 429.
	p.AddRule(Rule{
		Method: "POST",
		Path:   "/orders",
		Action: ActionRespond,
		Status: 429,
		Body:   `{"error":"rate_limited"}`,
	})

	// POST /orders → should get 429.
	resp, _ := http.Post("http://"+proxyAddr+"/orders", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 429 {
		t.Errorf("POST /orders status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "rate_limited") {
		t.Errorf("body = %q", string(body))
	}

	// GET /health → should forward (no matching rule).
	resp2, _ := http.Get("http://" + proxyAddr + "/health")
	if resp2.StatusCode != 200 {
		t.Errorf("GET /health status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()

	// Should have 1 proxy event.
	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
}

func TestHTTPProxyDelayRule(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTPProxy(nil, "test-svc")
	proxyAddr, _ := p.Start(ctx, mockLn.Addr().String())
	defer p.Stop()

	p.AddRule(Rule{
		Path:   "/slow*",
		Action: ActionDelay,
		Delay:  200 * time.Millisecond,
	})

	start := time.Now()
	resp, _ := http.Get("http://" + proxyAddr + "/slow-endpoint")
	elapsed := time.Since(start)
	resp.Body.Close()

	if elapsed < 150*time.Millisecond {
		t.Errorf("delay too short: %v", elapsed)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (forwarded after delay)", resp.StatusCode)
	}
}

func TestHTTPProxyPathGlob(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTPProxy(nil, "test-svc")
	proxyAddr, _ := p.Start(ctx, mockLn.Addr().String())
	defer p.Stop()

	p.AddRule(Rule{
		Path:   "/api/*",
		Action: ActionRespond,
		Status: 503,
	})

	// /api/orders → 503 (matches).
	resp, _ := http.Get("http://" + proxyAddr + "/api/orders")
	if resp.StatusCode != 503 {
		t.Errorf("/api/orders status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	// /health → 200 (doesn't match).
	resp2, _ := http.Get("http://" + proxyAddr + "/health")
	if resp2.StatusCode != 200 {
		t.Errorf("/health status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestManagerLifecycle(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	ctx := context.Background()
	mgr := NewManager(nil)
	defer mgr.StopAll()

	// Start proxy.
	addr, err := mgr.EnsureProxy(ctx, "gateway", "public", "http", mockLn.Addr().String())
	if err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if addr == "" {
		t.Fatal("expected non-empty proxy address")
	}

	// Second call returns same address.
	addr2, _ := mgr.EnsureProxy(ctx, "gateway", "public", "http", mockLn.Addr().String())
	if addr != addr2 {
		t.Errorf("expected same address, got %q vs %q", addr, addr2)
	}

	// Add rule and verify.
	mgr.AddRule("gateway", "public", Rule{
		Path:   "/orders",
		Action: ActionRespond,
		Status: 503,
	})

	resp, _ := http.Get("http://" + addr + "/orders")
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	// Clear rules — should forward.
	mgr.ClearRules("gateway", "public")
	resp2, _ := http.Get("http://" + addr + "/orders")
	if resp2.StatusCode != 200 {
		t.Errorf("after clear: status = %d, want 200", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestRuleMatchGlob(t *testing.T) {
	rule := Rule{Path: "/api/*", Method: "POST"}

	if !rule.MatchRequest("POST", "/api/orders", "", "", "", "") {
		t.Error("should match POST /api/orders")
	}
	if rule.MatchRequest("GET", "/api/orders", "", "", "", "") {
		t.Error("should not match GET")
	}
	if rule.MatchRequest("POST", "/health", "", "", "", "") {
		t.Error("should not match /health")
	}
}
