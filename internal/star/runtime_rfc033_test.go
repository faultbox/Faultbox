package star

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProxyActiveEventOnReusedService verifies the RFC-033 fix for Finding C:
// when startServices skips a service because its session was kept alive from
// a previous test (reuse=True), the runtime now re-emits one proxy_active
// event per running interface proxy. Without this, fault_matrix cells after
// the first show no proxy events at all and the trace looks like proxy
// lifecycle is broken.
func TestProxyActiveEventOnReusedService(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
api = service("api", "/tmp/mock",
    interface("main", "http", 8099),
    reuse = True,
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// Stand up a proxy directly — simulates the state left by a previous
	// fault_matrix cell where preStartProxies created the proxy fresh.
	_, err := rt.proxyMgr.EnsureProxy(context.Background(), "api", "main", "http", "127.0.0.1:8099")
	if err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	defer rt.proxyMgr.StopAll()

	// Simulate a kept-alive session — startServices' reuse check is
	// `_, running := rt.sessions[svcName]`, so any entry under the service
	// name triggers the reuse path. Zero value is fine: the reuse path
	// only checks presence, never reads session/cancel/done fields.
	rt.sessions["api"] = &runningSession{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Snapshot event count to scope our assertion to events emitted by
	// this startServices call.
	startIdx := rt.events.Len()

	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}

	emitted := rt.events.Events()[startIdx:]
	var found *Event
	for i := range emitted {
		ev := emitted[i]
		if ev.Type == "proxy_active" && ev.Service == "api" {
			found = &ev
			break
		}
	}
	if found == nil {
		t.Fatalf("expected proxy_active event for reused service, got: %v", eventTypes(emitted))
	}
	if got := found.Fields["interface"]; got != "main" {
		t.Errorf("proxy_active interface = %q, want %q", got, "main")
	}
	if got := found.Fields["protocol"]; got != "http" {
		t.Errorf("proxy_active protocol = %q, want %q", got, "http")
	}
	if got := found.Fields["mode"]; got != "reused" {
		t.Errorf("proxy_active mode = %q, want %q", got, "reused")
	}
	if got := found.Fields["listen"]; !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("proxy_active listen = %q, want host-loopback addr", got)
	}
}

// TestProxyActiveSkippedWhenNoProxyRegistered guards the empty-iface case:
// if a reused service has no proxy currently registered for an interface
// (e.g. protocol that doesn't support proxying, or proxy was torn down),
// proxy_active must not fire spuriously.
func TestProxyActiveSkippedWhenNoProxyRegistered(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
api = service("api", "/tmp/mock",
    interface("main", "http", 8099),
    reuse = True,
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// No EnsureProxy — proxy manager has nothing for api.main.
	rt.sessions["api"] = &runningSession{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	startIdx := rt.events.Len()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}

	emitted := rt.events.Events()[startIdx:]
	for _, ev := range emitted {
		if ev.Type == "proxy_active" {
			t.Errorf("unexpected proxy_active event when no proxy was registered: %+v", ev)
		}
	}
}

func eventTypes(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

// TestProxyAddrAttributePlaceholder verifies RFC-033 Phase 2: at spec-load
// time, iface.proxy_addr returns a placeholder string (the proxy doesn't
// exist yet). The customer's pattern of putting it into env values must
// survive any later string concatenation in the spec.
func TestProxyAddrAttributePlaceholder(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
db = service("db",
    interface("mysql", "tcp", 3306),
    interface("redis", "tcp", 6379),
    image = "mysql:8",
)
api = service("api", "/tmp/mock",
    interface("main", "http", 8000),
    env = {
        "MYSQL_HOST": db.mysql.proxy_host,
        "MYSQL_PORT": db.mysql.proxy_port,
        "MYSQL_DSN":  "user:pass@tcp(" + db.mysql.proxy_addr + ")/appdb",
        "REDIS_ADDR": db.redis.proxy_addr,
    },
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	api := rt.services["api"]
	if api == nil {
		t.Fatal("api service not registered")
	}

	// Spec-load values are placeholder sentinels — not real addresses.
	for k, v := range api.Env {
		if !strings.HasPrefix(v, "__FB_PROXY_") || !strings.HasSuffix(v, "__") {
			// The DSN value embeds the placeholder inside a longer string,
			// so it won't have the prefix/suffix — check that the placeholder
			// substring is present instead.
			if !strings.Contains(v, "__FB_PROXY_ADDR_db__mysql__") &&
				!strings.Contains(v, "__FB_PROXY_") {
				t.Errorf("%s = %q, want placeholder", k, v)
			}
		}
	}

	// Registry should record one entry per (svc, iface, kind) tuple. We
	// referenced db.mysql three times (host, port, addr) and db.redis once
	// (addr) — four unique placeholders.
	if got := len(rt.proxyPlaceholders); got != 4 {
		t.Errorf("len(proxyPlaceholders) = %d, want 4 (mysql host/port/addr + redis addr)", got)
	}
}

// TestProxyAddrResolvesAtBuildEnv verifies that once a proxy is running,
// buildEnv replaces the placeholder with the real host-side listener.
func TestProxyAddrResolvesAtBuildEnv(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
db = service("db",
    interface("mysql", "tcp", 3306),
    image = "mysql:8",
)
api = service("api", "/tmp/mock",
    interface("main", "http", 8000),
    env = {
        "MYSQL_HOST": db.mysql.proxy_host,
        "MYSQL_PORT": db.mysql.proxy_port,
        "MYSQL_ADDR": db.mysql.proxy_addr,
        "MYSQL_DSN":  "tcp(" + db.mysql.proxy_addr + ")/appdb",
    },
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// Stand up a proxy directly (skipping startContainerService — needs Docker).
	proxyAddr, err := rt.proxyMgr.EnsureProxy(context.Background(), "db", "mysql", "tcp", "127.0.0.1:3306")
	if err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	defer rt.proxyMgr.StopAll()

	envSlice := rt.buildEnv(rt.services["api"])
	envMap := make(map[string]string)
	for _, kv := range envSlice {
		i := strings.Index(kv, "=")
		envMap[kv[:i]] = kv[i+1:]
	}

	expectedHost, expectedPort, _ := splitHostPort(proxyAddr)
	if got := envMap["MYSQL_HOST"]; got != expectedHost {
		t.Errorf("MYSQL_HOST = %q, want %q", got, expectedHost)
	}
	if got := envMap["MYSQL_PORT"]; got != fmt.Sprintf("%d", expectedPort) {
		t.Errorf("MYSQL_PORT = %q, want %d", got, expectedPort)
	}
	if got := envMap["MYSQL_ADDR"]; got != proxyAddr {
		t.Errorf("MYSQL_ADDR = %q, want %q", got, proxyAddr)
	}
	wantDSN := fmt.Sprintf("tcp(%s)/appdb", proxyAddr)
	if got := envMap["MYSQL_DSN"]; got != wantDSN {
		t.Errorf("MYSQL_DSN = %q, want %q", got, wantDSN)
	}
}

// TestProxyAddrContainerConsumerRewritesHost verifies that container SUTs
// get host.docker.internal:port for proxy_addr / proxy_host (container can
// reach the host-side listener via the docker bridge but not via 127.0.0.1).
// proxy_port stays a numeric port either way.
func TestProxyAddrContainerConsumerRewritesHost(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
db = service("db",
    interface("mysql", "tcp", 3306),
    image = "mysql:8",
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// Register placeholders by accessing the attributes once.
	addrPH := rt.registerProxyPlaceholder("db", "mysql", "addr")
	hostPH := rt.registerProxyPlaceholder("db", "mysql", "host")
	portPH := rt.registerProxyPlaceholder("db", "mysql", "port")

	if _, err := rt.proxyMgr.EnsureProxy(context.Background(), "db", "mysql", "tcp", "127.0.0.1:3306"); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	defer rt.proxyMgr.StopAll()

	for name, mode := range map[string]consumerMode{"binary": binaryConsumer, "container": containerConsumer} {
		addr := rt.resolveProxyPlaceholders(addrPH, mode)
		host := rt.resolveProxyPlaceholders(hostPH, mode)
		port := rt.resolveProxyPlaceholders(portPH, mode)

		switch mode {
		case binaryConsumer:
			if !strings.HasPrefix(addr, "127.0.0.1:") {
				t.Errorf("[%s] addr = %q, want 127.0.0.1:<port>", name, addr)
			}
			if host != "127.0.0.1" {
				t.Errorf("[%s] host = %q, want 127.0.0.1", name, host)
			}
		case containerConsumer:
			if !strings.HasPrefix(addr, "host.docker.internal:") {
				t.Errorf("[%s] addr = %q, want host.docker.internal:<port>", name, addr)
			}
			if host != "host.docker.internal" {
				t.Errorf("[%s] host = %q, want host.docker.internal", name, host)
			}
		}

		if _, err := strconv.Atoi(port); err != nil {
			t.Errorf("[%s] port = %q, want numeric: %v", name, port, err)
		}
	}
}

// TestProxyAddrPlaceholderIdempotent verifies that referencing the same
// (svc, iface, kind) attribute multiple times produces the same placeholder
// (registry entry isn't duplicated, substitution still works).
func TestProxyAddrPlaceholderIdempotent(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
db = service("db",
    interface("mysql", "tcp", 3306),
    image = "mysql:8",
)
api = service("api", "/tmp/mock",
    interface("main", "http", 8000),
    env = {
        "A": db.mysql.proxy_addr,
        "B": db.mysql.proxy_addr,
        "C": db.mysql.proxy_addr,
    },
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	if got := len(rt.proxyPlaceholders); got != 1 {
		t.Errorf("len(proxyPlaceholders) = %d, want 1 (same attr × 3 refs)", got)
	}

	api := rt.services["api"]
	if api.Env["A"] != api.Env["B"] || api.Env["B"] != api.Env["C"] {
		t.Errorf("placeholder strings differ across refs: A=%q B=%q C=%q",
			api.Env["A"], api.Env["B"], api.Env["C"])
	}
}
