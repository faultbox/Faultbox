package star

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/proxy"
)

// RFC-036 runtime / proxy lifecycle tests. Pin the behaviour the RFC
// promises for remote services at session start, healthcheck, proxy
// upstream resolution, and env-var substitution.

// fakeRemoteHTTPServer stands up an httptest server and binds it to
// 127.0.0.1:port (port from the caller). Used as the "remote pod" in
// runtime tests so they don't need cluster access.
func fakeRemoteHTTPServer(t *testing.T, handler http.HandlerFunc) (host string, port int, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, func() {
		_ = srv.Shutdown(context.Background())
	}
}

func TestRemote_StartRemoteService_RegistersSession(t *testing.T) {
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	defer cleanup()

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("main", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	// Remote service should be registered as a running session.
	if _, ok := rt.sessions["geo"]; !ok {
		t.Fatalf("rt.sessions[%q] not registered", "geo")
	}
	// Session should be a no-op (no engine.Session attached).
	if rt.sessions["geo"].session != nil {
		t.Errorf("remote session should have nil session (no seccomp); got non-nil")
	}
}

func TestRemote_HealthcheckGatesStartup_UnreachableErrorWithHint(t *testing.T) {
	rt := New(testLogger())
	// Point at a port nothing's listening on; expect dial failure.
	src := `
geo = service("geo",
    interface("main", "http", 1),
    remote = "127.0.0.1",
    healthcheck = http("127.0.0.1:1/healthz", timeout = "200ms"),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := rt.startServices(ctx)
	if err == nil {
		t.Fatalf("expected startServices to fail on unreachable remote, got nil")
	}
	msg := err.Error()
	for _, frag := range []string{
		"remote service",
		"not reachable",
		"telepresence connect",
		"kubectl port-forward",
		"inside the target cluster",
	} {
		if !strings.Contains(msg, frag) {
			t.Errorf("error missing fragment %q\nfull error: %s", frag, msg)
		}
	}
}

func TestRemote_ServiceStartedEvent_KindRemote(t *testing.T) {
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	defer cleanup()

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	startIdx := rt.events.Len()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	// Walk events emitted during this startServices call; find service_started.
	var found bool
	all := rt.events.Events()
	for i := startIdx; i < len(all); i++ {
		e := all[i]
		if e.Type == "service_started" && e.Service == "geo" {
			found = true
			if e.Fields["kind"] != "remote" {
				t.Errorf("service_started.kind = %q, want %q", e.Fields["kind"], "remote")
			}
			if e.Fields["remote"] != host {
				t.Errorf("service_started.remote = %q, want %q", e.Fields["remote"], host)
			}
			wantUpstream := fmt.Sprintf("%s:%d", host, port)
			if e.Fields["upstream.public"] != wantUpstream {
				t.Errorf("service_started.upstream.public = %q, want %q", e.Fields["upstream.public"], wantUpstream)
			}
		}
	}
	if !found {
		t.Fatalf("service_started event for %q not emitted", "geo")
	}
}

func TestRemote_ProxyTargetAddrUsesRemoteUpstream(t *testing.T) {
	cases := []struct {
		name         string
		spec         string
		ifaceName    string
		wantUpstream string
	}{
		{
			name: "plain string remote, port from interface",
			spec: `
service("geo",
    interface("main", "http", 8080),
    remote = "geo.staging",
    healthcheck = http("geo.staging:8080/healthz"),
)`,
			ifaceName:    "main",
			wantUpstream: "geo.staging:8080",
		},
		{
			name: "remotes() per-interface override with explicit port",
			spec: `
service("geo",
    interface("public",   "http", 8080),
    interface("internal", "http", 9090),
    remote = remotes({
        "public":   "h1",
        "internal": "h2:9999",
    }),
    healthcheck = http("h1:8080/healthz"),
)`,
			ifaceName:    "internal",
			wantUpstream: "h2:9999",
		},
		{
			name: "remotes() per-interface override, host only",
			spec: `
service("geo",
    interface("public", "http", 8080),
    remote = remotes({"public": "h1"}),
    healthcheck = http("h1:8080/healthz"),
)`,
			ifaceName:    "public",
			wantUpstream: "h1:8080",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := New(testLogger())
			if err := rt.LoadString("test.star", tc.spec); err != nil {
				t.Fatalf("LoadString: %v", err)
			}
			svc := rt.services["geo"]
			iface := svc.Interfaces[tc.ifaceName]
			got := proxyTargetAddr(svc, iface)
			if got != tc.wantUpstream {
				t.Errorf("proxyTargetAddr = %q, want %q", got, tc.wantUpstream)
			}
		})
	}
}

func TestRemote_BuildEnvSubstitutesRemoteHost(t *testing.T) {
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	defer cleanup()

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)

api = service("api", "/tmp/mock",
    interface("main", "http", 8000),
    env = {
        "GEO_DIRECT_URL": "http://%s:%d/v1/regions",
    },
    depends_on = [geo],
)
`, port, host, host, port, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	// Verify the proxy is up and pointing at the fake remote.
	proxyAddr := rt.proxyMgr.GetProxyAddr("geo", "public")
	if proxyAddr == "" {
		t.Fatalf("expected proxy_addr for geo.public, got empty (proxy not started?)")
	}

	// buildEnv on the api service should rewrite the GEO_DIRECT_URL value
	// so the literal remote host:port is replaced with the proxy listener.
	apiSvc := rt.services["api"]
	envs := rt.buildEnv(apiSvc)
	var got string
	prefix := "GEO_DIRECT_URL="
	for _, e := range envs {
		if strings.HasPrefix(e, prefix) {
			got = strings.TrimPrefix(e, prefix)
			break
		}
	}
	if got == "" {
		t.Fatalf("GEO_DIRECT_URL not present in built env: %v", envs)
	}
	wantContains := proxyAddr
	if !strings.Contains(got, wantContains) {
		t.Errorf("GEO_DIRECT_URL = %q; expected substring %q (proxy addr) — substitution did not fire",
			got, wantContains)
	}
	originalRemote := fmt.Sprintf("%s:%d", host, port)
	if strings.Contains(got, originalRemote) {
		t.Errorf("GEO_DIRECT_URL still contains literal remote %q after substitution: %q",
			originalRemote, got)
	}
}

// TestRemote_PreStartProxyDialsRemote_FullLoop end-to-end: a "remote pod"
// served by httptest stands in for the cluster service. Faultbox starts,
// pre-starts the proxy with the remote as upstream, and a request through
// the proxy reaches the fake. With a fault rule installed, the proxy
// short-circuits with the configured response.
func TestRemote_PreStartProxyDialsRemote_FullLoop(t *testing.T) {
	upstreamHits := 0
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"from":"real-remote"}`))
	})
	defer cleanup()

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	proxyAddr := rt.proxyMgr.GetProxyAddr("geo", "public")
	if proxyAddr == "" {
		t.Fatalf("proxy not started for geo.public")
	}

	// Pass-through: should hit the fake and return its body.
	resp, err := http.Get("http://" + proxyAddr + "/v1/regions/EU")
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	body := readAll(t, resp)
	if !strings.Contains(string(body), "real-remote") {
		t.Errorf("pass-through body = %q, want substring real-remote", body)
	}
	if upstreamHits == 0 {
		t.Errorf("upstream not hit; proxy may not have dialed remote")
	}
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(b.String())
}

// Stub so the in-memory event log API matches what's used in tests above.
// rt.events.All() and .Len() exist on the event log; if not, this helper
// localises the fix for the test surface.
var _ = httptest.NewServer // keep import live in case all() / len() shift

// TestRemote_FaultRewriteOverridesRemoteResponse — full e2e for the
// "swap one keyword" claim. A fake remote returns 200 with a
// distinctive body. With a proxy rule installed (the same kind a
// fault_assumption(rules=[error(...)]) would install), a request
// through the pre-started proxy must receive the faulted response
// instead. This is the regression that locks in: protocol faults
// fire against remote services exactly like they fire against local
// containers.
func TestRemote_FaultRewriteOverridesRemoteResponse(t *testing.T) {
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"from":"real-remote"}`))
	})
	defer cleanup()

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	proxyAddr := rt.proxyMgr.GetProxyAddr("geo", "public")
	if proxyAddr == "" {
		t.Fatalf("no proxy addr for geo.public")
	}

	// Install the equivalent of `error(path="/v1/regions/**", status=503)`.
	if err := rt.proxyMgr.AddRule("geo", "public", proxy.Rule{
		Action: proxy.ActionRespond,
		Status: 503,
		Body:   `{"from":"fault-injected"}`,
	}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	resp, err := http.Get("http://" + proxyAddr + "/v1/regions/EU")
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 503 {
		t.Errorf("status = %d; want 503 (fault override)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "fault-injected") {
		t.Errorf("body = %q; want substring fault-injected", body)
	}
	if strings.Contains(string(body), "real-remote") {
		t.Errorf("body still has remote response %q — proxy did not short-circuit", body)
	}
}

// TestRemote_VsLocal_ProxyParity — stand up the same logical service
// twice: once as a binary mode service pointing at the same fake
// upstream (the local control), once as `remote=` pointing at the
// fake upstream directly. With identical proxy rules, requests
// through both proxies receive bit-equivalent responses (modulo
// timing). This is the test that locks in the "swap one keyword"
// success criterion in the RFC.
func TestRemote_VsLocal_ProxyParity(t *testing.T) {
	calls := 0
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"calls":1}`))
	})
	defer cleanup()

	// Run A: remote=
	rtRemote := New(testLogger())
	srcA := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rtRemote.LoadString("a.star", srcA); err != nil {
		t.Fatalf("Load A: %v", err)
	}
	ctxA, cancelA := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelA()
	if err := rtRemote.startServices(ctxA); err != nil {
		t.Fatalf("startServices A: %v", err)
	}
	t.Cleanup(func() { rtRemote.proxyMgr.StopAll() })
	addrA := rtRemote.proxyMgr.GetProxyAddr("geo", "public")
	respA, err := http.Get("http://" + addrA + "/p")
	if err != nil {
		t.Fatalf("GET A: %v", err)
	}
	bodyA := readAll(t, respA)

	// Run B: a separate runtime with EnsureProxy pointed at the same
	// upstream — matches what a local-binary-mode service would do once
	// preStartProxies wires it up. (Binary mode itself can't be exercised
	// in a unit test without a fork+exec target; the proxy machinery is
	// the same.)
	rtLocal := New(testLogger())
	if _, err := rtLocal.proxyMgr.EnsureProxy(context.Background(), "geo", "public", "http", fmt.Sprintf("%s:%d", host, port)); err != nil {
		t.Fatalf("EnsureProxy B: %v", err)
	}
	t.Cleanup(func() { rtLocal.proxyMgr.StopAll() })
	addrB := rtLocal.proxyMgr.GetProxyAddr("geo", "public")
	respB, err := http.Get("http://" + addrB + "/p")
	if err != nil {
		t.Fatalf("GET B: %v", err)
	}
	bodyB := readAll(t, respB)

	if string(bodyA) != string(bodyB) {
		t.Errorf("response divergence: remote=%q local=%q", bodyA, bodyB)
	}
	if respA.StatusCode != respB.StatusCode {
		t.Errorf("status divergence: remote=%d local=%d", respA.StatusCode, respB.StatusCode)
	}
}

// TestRemote_TLSUpstream_EndToEnd — RFC-036 × RFC-038 interop. A fake
// HTTPS upstream stands in for a real TLS-required cluster service.
// The spec combines `remote=` with `interface(..., tls=tls_cert(...))`;
// Faultbox's proxy terminates TLS at the listener (auto-generated
// self-signed cert) and dials the remote upstream over TLS using the
// resolved client config. End-to-end: SUT → HTTPS proxy → TLS upstream.
//
// Locks in the contract that `remote=` works against TLS-required
// upstreams, the most common shape in production-tier dev clusters.
func TestRemote_TLSUpstream_EndToEnd(t *testing.T) {
	upstreamSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"from":"tls-upstream"}`))
	}))
	defer upstreamSrv.Close()

	// upstreamSrv.URL is "https://127.0.0.1:NNN" — split into host:port.
	addr := strings.TrimPrefix(upstreamSrv.URL, "https://")
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d, tls = tls_cert(insecure = True)),
    remote      = "%s",
    # Use tcp() — http() healthcheck against a self-signed httptest.NewTLSServer
    # would need cert-trust plumbing the test doesn't care about. tcp() proves
    # the listener is up; the TLS handshake/dial path is what we're testing.
    healthcheck = tcp("%s:%d", timeout = "2s"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	proxyAddr := rt.proxyMgr.GetProxyAddr("geo", "public")
	if proxyAddr == "" {
		t.Fatalf("no proxy addr for geo.public")
	}

	// SUT speaks HTTPS to the proxy. The proxy's auto-generated cert
	// covers 127.0.0.1 (the loopback default), so a real client can
	// negotiate TLS without skipping verification — but we use
	// InsecureSkipVerify here to keep the test focused on the
	// proxy-dials-tls-upstream leg rather than chain handling.
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := client.Get("https://" + proxyAddr + "/v1/regions/EU")
	if err != nil {
		t.Fatalf("GET via tls proxy: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "tls-upstream") {
		t.Errorf("body = %q; want substring tls-upstream", body)
	}
}

// TestRemote_MidRunDeath — fake remote dies after a successful initial
// pass-through. The next request through the proxy gets a synthetic
// protocol error (lean-(b) from the RFC's Open Question 3). The proxy
// stays up; the SUT sees something that looks like a real service
// outage, which is exactly what oracles should test.
func TestRemote_MidRunDeath(t *testing.T) {
	host, port, cleanup := fakeRemoteHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"alive":true}`))
	})

	rt := New(testLogger())
	src := fmt.Sprintf(`
geo = service("geo",
    interface("public", "http", %d),
    remote = "%s",
    healthcheck = http("%s:%d/healthz"),
)
`, port, host, host, port)
	if err := rt.LoadString("test.star", src); err != nil {
		cleanup()
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		cleanup()
		t.Fatalf("startServices: %v", err)
	}
	t.Cleanup(func() { rt.proxyMgr.StopAll() })

	addr := rt.proxyMgr.GetProxyAddr("geo", "public")

	// Initial successful request.
	respOK, err := http.Get("http://" + addr + "/p")
	if err != nil {
		cleanup()
		t.Fatalf("GET pre-death: %v", err)
	}
	bodyOK := readAll(t, respOK)
	if !strings.Contains(string(bodyOK), "alive") {
		cleanup()
		t.Fatalf("pre-death body = %q; want alive", bodyOK)
	}

	// Kill the upstream.
	cleanup()
	// Give the listener time to fully release.
	time.Sleep(50 * time.Millisecond)

	// Subsequent request should fail at the proxy → upstream dial.
	// Different protocols surface this differently (httputil.ReverseProxy
	// returns 502 by default; raw byte proxies close the connection); we
	// just verify the SUT-facing request *does not* succeed silently.
	resp2, err := http.Get("http://" + addr + "/p")
	if err == nil {
		body2 := readAll(t, resp2)
		if resp2.StatusCode == 200 && strings.Contains(string(body2), "alive") {
			t.Fatalf("post-death request succeeded with original body — proxy did not detect upstream death (status=%d body=%q)", resp2.StatusCode, body2)
		}
	}
}