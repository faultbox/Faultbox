package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// h2cHandler is a tiny helper that wraps a HandlerFunc for the
// h2c upgrade path used by the plaintext HTTP/2 regression test.
func h2cHandler(fn http.HandlerFunc) http.Handler {
	return h2c.NewHandler(fn, &http2.Server{})
}

// startMockHTTPS starts a real upstream that speaks HTTPS using the
// auto-generated cert. Returns the addr (host:port) and a cleanup fn.
func startMockHTTPS(t *testing.T, handler http.Handler) (string, *tls.Config, func()) {
	t.Helper()
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	srv := &http.Server{Handler: handler}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	go srv.Serve(ln)
	return ln.Addr().String(), cfg, func() { srv.Close(); ln.Close() }
}

// httpsClientFor builds a TLS client that trusts the proxy's auto-cert
// (or the upstream's). ServerName=localhost matches the cert's SAN.
func httpsClientFor(t *testing.T, serverCfg *tls.Config) *http.Client {
	t.Helper()
	leaf, err := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "localhost",
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// TestHTTPProxy_TLSEndToEnd exercises the headline RFC-038 case:
// client speaks HTTPS to the proxy, proxy speaks HTTPS to the
// upstream, plaintext rule-matching keeps working in the middle.
func TestHTTPProxy_TLSEndToEnd(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startMockHTTPS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok-from-upstream")
	}))
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
	serverCfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("server cert: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTPProxy(nil, "test-svc")
	p.SetTLS(serverCfg, clientCfg)
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	client := httpsClientFor(t, serverCfg)
	resp, err := client.Get("https://" + proxyAddr + "/hello")
	if err != nil {
		t.Fatalf("HTTPS through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok-from-upstream" {
		t.Errorf("body = %q", body)
	}
}

// TestHTTPProxy_TLSRuleInjection — fault rules still match against
// the plaintext seen between the two TLS legs. This is the entire
// reason RFC-038 picked Option B over Option C (TLS-blind tunnel).
func TestHTTPProxy_TLSRuleInjection(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startMockHTTPS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "should not reach upstream")
	}))
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTPProxy(nil, "test-svc")
	p.SetTLS(serverCfg, clientCfg)
	proxyAddr, _ := p.Start(ctx, upstreamAddr)
	defer p.Stop()

	p.AddRule(Rule{
		Method: "GET",
		Path:   "/blocked",
		Action: ActionRespond,
		Status: 503,
		Body:   `{"error":"injected"}`,
	})

	client := httpsClientFor(t, serverCfg)
	resp, err := client.Get("https://" + proxyAddr + "/blocked")
	if err != nil {
		t.Fatalf("HTTPS: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "injected") {
		t.Errorf("body = %q, want injected", body)
	}
}

// TestHTTPProxy_PlaintextStillWorks — regression check. The opt-in
// TLS path must not break the existing plain-TCP plugin lifecycle.
// SetTLS is never called, plugin behaves exactly like pre-RFC-038.
func TestHTTPProxy_PlaintextStillWorks(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "plaintext")
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newHTTPProxy(nil, "test-svc")
	proxyAddr, _ := p.Start(ctx, mockLn.Addr().String())
	defer p.Stop()

	resp, err := http.Get("http://" + proxyAddr + "/")
	if err != nil {
		t.Fatalf("plain http: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "plaintext" {
		t.Errorf("body = %q", body)
	}
}

// TestHTTPProxy_ImplementsTLSAware — Manager.EnsureProxyTLS uses
// type assertion to detect TLS support; this pins the contract.
func TestHTTPProxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*httpProxy)(nil)
}

// TestHTTP2Proxy_TLSEndToEnd — same as the HTTP/1 case but for the
// HTTP/2 plugin. Upstream speaks h2-over-TLS; proxy terminates and
// re-establishes h2-over-TLS to the upstream. ALPN is negotiated
// at both legs.
func TestHTTP2Proxy_TLSEndToEnd(t *testing.T) {
	upstreamCfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("upstream cert: %v", err)
	}
	upstreamCfg.NextProtos = []string{"h2"}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			fmt.Fprint(w, "h2-from-upstream proto="+r.Proto)
		}),
	}
	if err := http2.ConfigureServer(srv, &http2.Server{}); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	upstreamLn, err := tls.Listen("tcp", "127.0.0.1:0", upstreamCfg)
	if err != nil {
		t.Fatalf("upstream tls.Listen: %v", err)
	}
	go srv.Serve(upstreamLn)
	defer srv.Close()
	defer upstreamLn.Close()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTP2Proxy(nil, "test-svc")
	p.SetTLS(serverCfg, clientCfg)
	proxyAddr, err := p.Start(ctx, upstreamLn.Addr().String())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    clientPool,
			ServerName: "localhost",
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + proxyAddr + "/")
	if err != nil {
		t.Fatalf("HTTP/2 through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "HTTP/2.0") {
		t.Errorf("upstream did not see HTTP/2: body=%q", body)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Errorf("client side proto = %q, want HTTP/2.0", resp.Proto)
	}
}

// TestHTTP2Proxy_TLSRuleInjection — fault rule fires inside the TLS
// tunnel just like the HTTP/1 case.
func TestHTTP2Proxy_TLSRuleInjection(t *testing.T) {
	upstreamCfg, _ := GenerateSelfSignedCert(nil)
	upstreamCfg.NextProtos = []string{"h2"}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			fmt.Fprint(w, "should not reach upstream")
		}),
	}
	_ = http2.ConfigureServer(srv, &http2.Server{})
	upstreamLn, _ := tls.Listen("tcp", "127.0.0.1:0", upstreamCfg)
	go srv.Serve(upstreamLn)
	defer srv.Close()
	defer upstreamLn.Close()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newHTTP2Proxy(nil, "test-svc")
	p.SetTLS(serverCfg, clientCfg)
	proxyAddr, _ := p.Start(ctx, upstreamLn.Addr().String())
	defer p.Stop()

	p.AddRule(Rule{
		Method: "GET",
		Path:   "/blocked",
		Action: ActionRespond,
		Status: 503,
		Body:   `{"error":"injected"}`,
	})

	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    clientPool,
			ServerName: "localhost",
			NextProtos: []string{"h2"},
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + proxyAddr + "/blocked")
	if err != nil {
		t.Fatalf("h2 through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), "injected") {
		t.Errorf("body = %q", body)
	}
}

// TestHTTP2Proxy_PlaintextStillWorks — regression guard on the h2c
// path. Without SetTLS the proxy speaks h2c upstream (matches the
// pre-RFC-038 behaviour); we point it at an h2c upstream and verify
// a round trip.
func TestHTTP2Proxy_PlaintextStillWorks(t *testing.T) {
	// h2c upstream: http.Server wrapped by h2c.NewHandler so it
	// speaks HTTP/2 over cleartext (matches the proxy's transport
	// expectation when no clientTLS is set).
	upstream := &http.Server{}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	upstream.Handler = h2cHandler(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "h2c-plain")
	})
	go upstream.Serve(mockLn)
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newHTTP2Proxy(nil, "test-svc")
	proxyAddr, _ := p.Start(ctx, mockLn.Addr().String())
	defer p.Stop()

	// Use an h2c client so we exercise the same path as before.
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(_ context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.Dial(network, addr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + proxyAddr + "/")
	if err != nil {
		t.Fatalf("h2c through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h2c-plain" {
		t.Errorf("body = %q", body)
	}
}

// TestHTTP2Proxy_ImplementsTLSAware pins the contract.
func TestHTTP2Proxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*http2Proxy)(nil)
}

// TestEnsureProxyTLS_AppliedFlag — the runtime depends on the
// tlsApplied return to decide whether to emit proxy_tls_pending.
// http supports TLS, tcp doesn't (yet) — verify both signals.
func TestEnsureProxyTLS_AppliedFlag(t *testing.T) {
	mock := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})}
	mockLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go mock.Serve(mockLn)
	defer mock.Close()

	mgr := NewManager(nil)
	defer mgr.StopAll()

	// http: implements TLSAware → applied=true.
	serverCfg, _ := GenerateSelfSignedCert(nil)
	_, applied, err := mgr.EnsureProxyTLS(context.Background(), "svc", "http-iface", "http", mockLn.Addr().String(), serverCfg, nil)
	if err != nil {
		t.Fatalf("http EnsureProxyTLS: %v", err)
	}
	if !applied {
		t.Errorf("http: expected tlsApplied=true")
	}

	// tcp: does NOT implement TLSAware → applied=false.
	_, applied, err = mgr.EnsureProxyTLS(context.Background(), "svc", "tcp-iface", "tcp", mockLn.Addr().String(), serverCfg, nil)
	if err != nil {
		t.Fatalf("tcp EnsureProxyTLS: %v", err)
	}
	if applied {
		t.Errorf("tcp: expected tlsApplied=false (plugin not migrated yet)")
	}
}
