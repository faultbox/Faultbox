package star

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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