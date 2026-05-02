package star

import (
	"strings"
	"testing"
)

// RFC-036 spec-load validation tests. These pin every behaviour the RFC
// promises so future refactors (and the eventual RFC-037 changes) can move
// fast without re-litigating them.

// loadStringErr loads a Starlark spec and returns the error (or nil) that
// the runtime produced. Tests use this instead of t.Fatalf-on-error
// because the unit under test is the error path.
func loadStringErr(t *testing.T, src string) error {
	t.Helper()
	rt := New(testLogger())
	return rt.LoadString("test.star", src)
}

func mustContain(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	for _, f := range fragments {
		if !strings.Contains(msg, f) {
			t.Fatalf("error %q missing fragment %q", msg, f)
		}
	}
}

// ---- Mutual-exclusion of source kwargs ----

func TestRemote_RejectsBinaryAndRemote(t *testing.T) {
	err := loadStringErr(t, `
service("svc", "/usr/bin/x",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
)
`)
	mustContain(t, err, "service()", "only one of", "remote")
}

func TestRemote_RejectsImageAndRemote(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    image = "redis:7",
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
)
`)
	mustContain(t, err, "service()", "only one of", "remote")
}

func TestRemote_RejectsBuildAndRemote(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    build = "./svc",
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
)
`)
	mustContain(t, err, "service()", "only one of", "remote")
}

func TestRemote_RequiresAtLeastOneSource(t *testing.T) {
	err := loadStringErr(t, `
service("svc", interface("main", "http", 8080))
`)
	mustContain(t, err, "service()", "requires one of", "remote=")
}

// ---- Healthcheck mandatoriness ----

func TestRemote_RequiresHealthcheck(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
)
`)
	mustContain(t, err, `service() "svc" is remote`, "healthcheck=")
}

// ---- Incompatible kwargs ----

func TestRemote_RejectsSeed(t *testing.T) {
	err := loadStringErr(t, `
def _seed(): pass
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    seed = _seed,
)
`)
	mustContain(t, err, "remote", "seed=", "not supported")
}

func TestRemote_RejectsReset(t *testing.T) {
	err := loadStringErr(t, `
def _reset(): pass
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    reset = _reset,
)
`)
	mustContain(t, err, "remote", "reset=", "not supported")
}

func TestRemote_RejectsReuse(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    reuse = True,
)
`)
	mustContain(t, err, "remote", "reuse=", "not supported")
}

func TestRemote_RejectsVolumes(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    volumes = {"/host": "/container"},
)
`)
	mustContain(t, err, "remote", "volumes=", "not supported")
}

func TestRemote_RejectsPorts(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    ports = {8080: 9090},
)
`)
	mustContain(t, err, "remote", "ports=", "not supported")
}

func TestRemote_RejectsArgs(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    args = ["--debug"],
)
`)
	mustContain(t, err, "remote", "args=", "not supported")
}

func TestRemote_RejectsSeccomp(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    seccomp = False,
)
`)
	mustContain(t, err, "remote", "seccomp=", "not supported")
}

func TestRemote_RejectsObserve(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    observe = [stdout()],
)
`)
	mustContain(t, err, "remote", "observe=", "not supported")
}

func TestRemote_RejectsOps(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
    ops = {"persist": op(syscalls = ["write"], path = "/tmp/*.wal")},
)
`)
	mustContain(t, err, "remote", "ops=", "syscall-level")
}

// ---- remote= type-checking ----

func TestRemote_RejectsEmptyString(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = "",
    healthcheck = http("x:8080/healthz"),
)
`)
	mustContain(t, err, "service()", "remote", "non-empty")
}

func TestRemote_RejectsNonStringNonRemotes(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("main", "http", 8080),
    remote = 42,
    healthcheck = http("x:8080/healthz"),
)
`)
	mustContain(t, err, "service()", "remote", "string", "remotes")
}

// ---- remotes({...}) typed value ----

func TestRemote_RemotesAcceptsValidDict(t *testing.T) {
	err := loadStringErr(t, `
svc = service("svc",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote = remotes({
        "public":   "host.example",
        "internal": "host.example:9090",
    }),
    healthcheck = http("host.example:8080/healthz"),
)
`)
	if err != nil {
		t.Fatalf("expected ok, got error: %v", err)
	}
}

func TestRemote_RemotesRejectsUnknownInterface(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    interface("public", "http", 8080),
    remote = remotes({"public": "h", "ghost": "h"}),
    healthcheck = http("h:8080/healthz"),
)
`)
	mustContain(t, err, `service() "svc"`, "remotes({...})", `"ghost"`, "not declared")
}

func TestRemote_RemotesRejectsEmptyDict(t *testing.T) {
	err := loadStringErr(t, `remotes({})`)
	mustContain(t, err, "remotes()", "at least one entry")
}

func TestRemote_RemotesRejectsEmptyHost(t *testing.T) {
	err := loadStringErr(t, `remotes({"public": ""})`)
	mustContain(t, err, "remotes()", "non-empty", "public")
}

func TestRemote_RemotesRejectsNonStringValue(t *testing.T) {
	err := loadStringErr(t, `remotes({"public": 42})`)
	mustContain(t, err, "remotes()", "must be strings", "public")
}

// ---- Happy path: remote-only service loads cleanly ----

func TestRemote_PlainStringHostHappyPath(t *testing.T) {
	err := loadStringErr(t, `
geo = service("geo-config",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote = "geo-config.staging.svc.cluster.local",
    healthcheck = http("geo-config.staging.svc.cluster.local:8080/healthz"),
)
`)
	if err != nil {
		t.Fatalf("expected ok, got error: %v", err)
	}
}

func TestRemote_RequiresAtLeastOneInterface(t *testing.T) {
	err := loadStringErr(t, `
service("svc",
    remote = "host.example",
    healthcheck = http("host.example:8080/healthz"),
)
`)
	mustContain(t, err, `service() "svc"`, "remote", "at least one interface()")
}

// ---- Field plumbing — IsRemote() reflects parsed state ----

func TestRemote_IsRemoteForPlainString(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
service("svc",
    interface("main", "http", 8080),
    remote = "h",
    healthcheck = http("h:8080/healthz"),
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	svc, ok := rt.services["svc"]
	if !ok {
		t.Fatalf("service not registered")
	}
	if !svc.IsRemote() {
		t.Fatalf("IsRemote() = false; expected true (Remote=%q)", svc.Remote)
	}
	if svc.Remote != "h" {
		t.Fatalf("Remote = %q; want %q", svc.Remote, "h")
	}
	if svc.RemoteHostFor("main") != "h" {
		t.Fatalf("RemoteHostFor(main) = %q; want %q", svc.RemoteHostFor("main"), "h")
	}
}

func TestRemote_IsRemoteForPerInterface(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
service("svc",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote = remotes({"public": "h1", "internal": "h2:9090"}),
    healthcheck = http("h1:8080/healthz"),
)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	svc := rt.services["svc"]
	if !svc.IsRemote() {
		t.Fatalf("IsRemote() = false; expected true")
	}
	if svc.Remote != "" {
		t.Fatalf("Remote = %q; want empty (per-interface form)", svc.Remote)
	}
	if got := svc.RemoteHostFor("public"); got != "h1" {
		t.Fatalf("RemoteHostFor(public) = %q; want h1", got)
	}
	if got := svc.RemoteHostFor("internal"); got != "h2:9090" {
		t.Fatalf("RemoteHostFor(internal) = %q; want h2:9090", got)
	}
}

func TestRemote_IsRemoteFalseForLocal(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
service("svc", "/usr/bin/x", interface("main", "http", 8080))
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if rt.services["svc"].IsRemote() {
		t.Fatalf("IsRemote() = true; expected false for binary service")
	}
}

// ---- Fault rule rejection — fault_assumption() ----

func TestRemote_FaultAssumption_RejectsSyscallFault(t *testing.T) {
	err := loadStringErr(t, `
geo = service("geo",
    interface("public", "http", 8080),
    remote = "geo.example",
    healthcheck = http("geo.example:8080/healthz"),
)

fault_assumption("net_blip",
    target = geo,
    write  = deny("EIO"),
)
`)
	mustContain(t, err,
		`fault_assumption("net_blip")`,
		`is remote`,
		"syscall-level faults",
		"write",
		"rules=",
	)
}

func TestRemote_FaultAssumption_AcceptsProtocolRules(t *testing.T) {
	err := loadStringErr(t, `
geo = service("geo",
    interface("public", "http", 8080),
    remote = "geo.example",
    healthcheck = http("geo.example:8080/healthz"),
)

fault_assumption("geo_unavailable",
    target = geo.public,
    rules  = [error(path = "/v1/regions/**", status = 503)],
)
`)
	if err != nil {
		t.Fatalf("expected ok, got error: %v", err)
	}
}

// ---- @faultbox/discovery/k8s.star helper ----

func TestK8sDiscovery_ServiceReturnsClusterDNS(t *testing.T) {
	rt := New(testLogger())
	src := `
load("@faultbox/discovery/k8s.star", "k8s")

geo = service("geo-config",
    interface("public", "http", 8080),
    remote      = k8s.service("geo-config", namespace = "staging"),
    healthcheck = http(k8s.endpoint("geo-config", 8080, namespace = "staging") + "/healthz"),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	svc := rt.services["geo-config"]
	if svc == nil {
		t.Fatalf("service not registered")
	}
	want := "geo-config.staging.svc.cluster.local"
	if svc.Remote != want {
		t.Errorf("svc.Remote = %q; want %q", svc.Remote, want)
	}
	wantHC := "http://geo-config.staging.svc.cluster.local:8080/healthz"
	if svc.Healthcheck.Test != wantHC {
		t.Errorf("healthcheck = %q; want %q", svc.Healthcheck.Test, wantHC)
	}
}

func TestK8sDiscovery_DefaultNamespace(t *testing.T) {
	rt := New(testLogger())
	src := `
load("@faultbox/discovery/k8s.star", "k8s")

svc = service("svc",
    interface("main", "http", 8080),
    remote      = k8s.service("svc"),
    healthcheck = http(k8s.endpoint("svc", 8080) + "/h"),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if rt.services["svc"].Remote != "svc.default.svc.cluster.local" {
		t.Errorf("default namespace path: %q", rt.services["svc"].Remote)
	}
}

func TestK8sDiscovery_LocalShortForm(t *testing.T) {
	rt := New(testLogger())
	src := `
load("@faultbox/discovery/k8s.star", "k8s")

svc = service("svc",
    interface("main", "http", 8080),
    remote      = "geo.staging",
    healthcheck = http(k8s.local("geo", 8080, namespace = "staging") + "/h"),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	wantHC := "http://geo.staging:8080/h"
	if rt.services["svc"].Healthcheck.Test != wantHC {
		t.Errorf("local short form: %q; want %q", rt.services["svc"].Healthcheck.Test, wantHC)
	}
}

func TestRemote_FaultAssumption_RejectsSyscallEvenWithIfaceTarget(t *testing.T) {
	// Targeting an interface_ref still has an underlying service; the gate
	// must catch this too, not just bare service() targets.
	err := loadStringErr(t, `
geo = service("geo",
    interface("public", "http", 8080),
    remote = "geo.example",
    healthcheck = http("geo.example:8080/healthz"),
)

fault_assumption("disk_blip",
    target = geo.public,
    write  = deny("EIO"),
)
`)
	mustContain(t, err, "is remote", "syscall-level faults", "write")
}
