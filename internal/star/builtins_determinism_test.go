package star

import (
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/engine"
)

// RFC-040 §8.4 reserved-syntax + §8.2 escape-hatch parse-time tests. Pinned
// here so future runtime/level migrations don't accidentally relax the
// gating that the RFC promises.

// ---- Defaults (no determinism() call) ----

func TestDeterminism_DefaultsWhenNotDeclared(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `service("svc", "/bin/true", interface("main", "http", 8080))`)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}
	if rt.detLevel != DeterminismL1 {
		t.Errorf("default detLevel = %q; want %q", rt.detLevel, DeterminismL1)
	}
	if rt.detRuntime != DeterminismRuntimeDefault {
		t.Errorf("default detRuntime = %q; want %q", rt.detRuntime, DeterminismRuntimeDefault)
	}
	if !rt.detStrict {
		t.Error("default detStrict = false; want true")
	}
	if rt.detExplicit {
		t.Error("detExplicit should be false when determinism() not called")
	}
}

// ---- Happy paths ----

func TestDeterminism_AcceptsL0(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(level = "L0")`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.detLevel != DeterminismL0 {
		t.Errorf("detLevel = %q; want L0", rt.detLevel)
	}
	if !rt.detExplicit {
		t.Error("detExplicit should be true after determinism() call")
	}
}

func TestDeterminism_AcceptsL1WithStrictAndAllow(t *testing.T) {
	rt := New(testLogger())
	src := `determinism(level = "L1", runtime = "default", strict = False, allow = ["clock", "dns"])`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt.detStrict {
		t.Error("detStrict should be false (set explicitly)")
	}
	if !rt.detAllow[CategoryClock] || !rt.detAllow[CategoryDNS] {
		t.Errorf("detAllow = %v; want clock+dns", rt.detAllow)
	}
	if rt.detAllow[CategoryRand] {
		t.Error("detAllow contains rand; should not")
	}
}

// ---- Reserved-syntax gating: levels ----

func TestDeterminism_RejectsL2(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "L2")`)
	mustContain(t, err, "determinism(level=", "L2", "reserved", "RFC-046")
}

func TestDeterminism_RejectsL3(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "L3")`)
	mustContain(t, err, "L3", "reserved")
}

func TestDeterminism_RejectsL4(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "L4")`)
	mustContain(t, err, "L4", "reserved")
}

func TestDeterminism_RejectsL5(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "L5")`)
	mustContain(t, err, "L5", "reserved")
}

func TestDeterminism_RejectsBogusLevel(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "tier-3")`)
	mustContain(t, err, "tier-3", "must be one of")
}

// ---- Reserved-syntax gating: runtime ----

func TestDeterminism_RejectsRuntimeGVisor(t *testing.T) {
	err := loadStringErr(t, `determinism(runtime = "gvisor")`)
	mustContain(t, err, "runtime=", "gvisor", "reserved", "RFC-046")
}

func TestDeterminism_RejectsBogusRuntime(t *testing.T) {
	err := loadStringErr(t, `determinism(runtime = "wasm")`)
	mustContain(t, err, "runtime=", "wasm")
}

// ---- strict semantics ----

func TestDeterminism_StrictRejectedAtL0(t *testing.T) {
	err := loadStringErr(t, `determinism(level = "L0", strict = True)`)
	mustContain(t, err, "strict", "L0", "no violation events exist")
}

func TestDeterminism_StrictRejectsNonBool(t *testing.T) {
	err := loadStringErr(t, `determinism(strict = "yes")`)
	mustContain(t, err, "strict", "must be a bool")
}

// ---- allow validation ----

func TestDeterminism_RejectsUnknownCategory(t *testing.T) {
	err := loadStringErr(t, `determinism(allow = ["clock", "shenanigans"])`)
	mustContain(t, err, "allow=", "shenanigans", "unknown category")
}

func TestDeterminism_RejectsAllowNonString(t *testing.T) {
	err := loadStringErr(t, `determinism(allow = ["clock", 42])`)
	mustContain(t, err, "allow=", "must be strings")
}

func TestDeterminism_RejectsAllowNonList(t *testing.T) {
	err := loadStringErr(t, `determinism(allow = "clock")`)
	mustContain(t, err, "allow=", "list of category strings")
}

func TestDeterminism_AcceptsAllFiveCategories(t *testing.T) {
	src := `determinism(allow = ["clock", "rand", "dns", "network-unmediated", "fs-unmediated"])`
	rt := New(testLogger())
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cat := range KnownCategories {
		if !rt.detAllow[cat] {
			t.Errorf("detAllow missing %q", cat)
		}
	}
}

// ---- Single-call constraint ----

func TestDeterminism_RejectsDoubleCall(t *testing.T) {
	err := loadStringErr(t, `
determinism(level = "L1")
determinism(level = "L0")
`)
	mustContain(t, err, "may only be called once")
}

// ---- Positional args rejected ----

func TestDeterminism_RejectsPositionalArgs(t *testing.T) {
	err := loadStringErr(t, `determinism("L1")`)
	mustContain(t, err, "keyword arguments")
}

// ---- service(nondeterministic_ok=...) ----

func TestServiceNondeterministicOK_AcceptsKnownCategories(t *testing.T) {
	rt := New(testLogger())
	src := `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = ["clock", "dns"],
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svc := rt.services["api"]
	if svc == nil {
		t.Fatalf("service 'api' not registered")
	}
	if !svc.NondeterministicOK[CategoryClock] || !svc.NondeterministicOK[CategoryDNS] {
		t.Errorf("NondeterministicOK = %v; want clock+dns", svc.NondeterministicOK)
	}
}

func TestServiceNondeterministicOK_RejectsUnknownCategory(t *testing.T) {
	err := loadStringErr(t, `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = ["clock", "made-up"],
)
`)
	mustContain(t, err, `service() "api"`, "nondeterministic_ok", "made-up", "unknown category")
}

func TestServiceNondeterministicOK_RejectsNonList(t *testing.T) {
	err := loadStringErr(t, `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = "clock",
)
`)
	mustContain(t, err, `service() "api"`, "nondeterministic_ok", "list of category strings")
}

func TestServiceNondeterministicOK_RejectsNonStringItems(t *testing.T) {
	err := loadStringErr(t, `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = [42],
)
`)
	mustContain(t, err, `service() "api"`, "nondeterministic_ok", "items must be strings")
}

// ---- effectiveAllow union ----

func TestEffectiveAllow_UnionsSpecAndService(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(allow = ["clock"])
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = ["dns"],
)
service("worker", "/bin/true",
    interface("main", "http", 8081),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	apiAllow := rt.effectiveAllow("api")
	if !apiAllow[CategoryClock] || !apiAllow[CategoryDNS] {
		t.Errorf("api effectiveAllow = %v; want clock+dns", apiAllow)
	}
	if apiAllow[CategoryRand] {
		t.Error("api effectiveAllow contains rand; should not")
	}

	workerAllow := rt.effectiveAllow("worker")
	if !workerAllow[CategoryClock] {
		t.Error("worker should inherit spec-level clock")
	}
	if workerAllow[CategoryDNS] {
		t.Error("worker should NOT see api's dns")
	}
}

func TestEffectiveAllow_UnknownService(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(allow = ["clock"])`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	allow := rt.effectiveAllow("nonexistent")
	if !allow[CategoryClock] {
		t.Error("unknown service should still see spec-level allow")
	}
}

// ---- Detection: isMediatedAddress ----

func TestIsMediatedAddress_DeclaredInterface(t *testing.T) {
	rt := New(testLogger())
	src := `service("api", "/bin/true", interface("main", "http", 8080))`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rt.isMediatedAddress("127.0.0.1", 8080) {
		t.Error("declared interface port 8080 should be mediated")
	}
	if rt.isMediatedAddress("127.0.0.1", 8081) {
		t.Error("undeclared port 8081 should not be mediated")
	}
}

func TestIsMediatedAddress_ZeroPort(t *testing.T) {
	rt := New(testLogger())
	if rt.isMediatedAddress("127.0.0.1", 0) {
		t.Error("port 0 must always be unmediated")
	}
}

// ---- Detection: detectUnmediated emits unmediated_io ----

func TestDetectUnmediated_EmitsClock(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall: "clock_gettime",
		PID:     1234,
	})
	got := findEvent(rt, "unmediated_io")
	if got == nil {
		t.Fatalf("expected unmediated_io event, got none")
	}
	if got.Fields["category"] != "clock" {
		t.Errorf("category = %q; want clock", got.Fields["category"])
	}
	if got.Fields["syscall"] != "clock_gettime" {
		t.Errorf("syscall = %q; want clock_gettime", got.Fields["syscall"])
	}
}

func TestDetectUnmediated_EmitsRand(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall: "getrandom",
		PID:     1234,
	})
	got := findEvent(rt, "unmediated_io")
	if got == nil || got.Fields["category"] != "rand" {
		t.Fatalf("expected category=rand, got %v", got)
	}
}

func TestDetectUnmediated_ConnectToDeclaredInterface_Silent(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall:  "connect",
		PID:      1234,
		DestIP:   "127.0.0.1",
		DestPort: 8080, // declared interface port
	})
	if got := findEvent(rt, "unmediated_io"); got != nil {
		t.Errorf("connect to mediated address should be silent; got %v", got)
	}
}

func TestDetectUnmediated_ConnectToUndeclaredPort_Network(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall:  "connect",
		PID:      1234,
		DestIP:   "127.0.0.1",
		DestPort: 9999, // not declared, not 53
	})
	got := findEvent(rt, "unmediated_io")
	if got == nil || got.Fields["category"] != "network-unmediated" {
		t.Fatalf("expected category=network-unmediated, got %v", got)
	}
	if !strings.Contains(got.Fields["detail"], "127.0.0.1:9999") {
		t.Errorf("detail should include addr:port; got %q", got.Fields["detail"])
	}
}

func TestDetectUnmediated_ConnectToPort53_DNS(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall:  "connect",
		PID:      1234,
		DestIP:   "8.8.8.8",
		DestPort: 53,
	})
	got := findEvent(rt, "unmediated_io")
	if got == nil || got.Fields["category"] != "dns" {
		t.Fatalf("expected category=dns, got %v", got)
	}
}

func TestDetectUnmediated_ConnectMissingDest_Silent(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// sockaddr read failed upstream; no DestIP populated.
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall: "connect",
		PID:     1234,
	})
	if got := findEvent(rt, "unmediated_io"); got != nil {
		t.Errorf("missing DestIP should be silent (cannot classify); got %v", got)
	}
}

func TestDetectUnmediated_ProxyListenerIsMediated(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `service("api", "/bin/true", interface("main", "http", 8080))`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Register a proxy listener; the SUT dialing it should not fire.
	rt.proxyMgr.RegisterListenAddr("api", "main", "127.0.0.1:34567")
	rt.detectUnmediated("api", engine.SyscallEvent{
		Syscall:  "connect",
		PID:      1234,
		DestIP:   "127.0.0.1",
		DestPort: 34567,
	})
	if got := findEvent(rt, "unmediated_io"); got != nil {
		t.Errorf("connect to Faultbox proxy port should be silent; got %v", got)
	}
}

func TestDetectUnmediated_L0SkipsAllDetection(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(level = "L0")
service("api", "/bin/true", interface("main", "http", 8080))
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "clock_gettime", PID: 1234})
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "getrandom", PID: 1234})
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "connect", PID: 1234, DestIP: "8.8.8.8", DestPort: 53})
	if got := findEvent(rt, "unmediated_io"); got != nil {
		t.Errorf("L0 should not emit any unmediated_io events; got %v", got)
	}
}

// findEvent returns the first event with the given Type from the runtime's
// event log, or nil if none. Test helper.
func findEvent(rt *Runtime, eventType string) *Event {
	all := rt.events.Events()
	for i := range all {
		ev := &all[i]
		if ev.Type == eventType {
			return ev
		}
	}
	return nil
}

// ---- Strict-mode helpers (RFC-040 §8.3) ----

func TestStrictEffective_DefaultsTrueAtL1(t *testing.T) {
	rt := New(testLogger())
	if !rt.strictEffective() {
		t.Error("strictEffective should default true at L1 (no override)")
	}
}

func TestStrictEffective_FalseAtL0(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(level = "L0")`); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if rt.strictEffective() {
		t.Error("strictEffective must be false at L0")
	}
}

func TestStrictEffective_FollowsSpecStrictFalse(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(strict = False)`); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if rt.strictEffective() {
		t.Error("strictEffective should be false when spec sets strict=False")
	}
}

func TestStrictEffective_OverrideWins(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(strict = False)`); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	yes := true
	rt.detStrictOverride = &yes
	if !rt.strictEffective() {
		t.Error("override=true should beat spec strict=False")
	}
	no := false
	rt.detStrictOverride = &no
	if rt.strictEffective() {
		t.Error("override=false should beat any spec setting")
	}
}

func TestFirstStrictViolation_FindsUntoleratedCategory(t *testing.T) {
	rt := New(testLogger())
	src := `service("api", "/bin/true", interface("main", "http", 8080))`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "clock_gettime", PID: 1234})

	v := rt.firstStrictViolation(rt.events.Events())
	if v == nil {
		t.Fatalf("expected a violation; got nil")
	}
	if v.Fields["category"] != "clock" {
		t.Errorf("category = %q; want clock", v.Fields["category"])
	}
}

func TestFirstStrictViolation_TolerateViaSpec(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(allow = ["clock"])
service("api", "/bin/true", interface("main", "http", 8080))
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "clock_gettime", PID: 1234})

	if v := rt.firstStrictViolation(rt.events.Events()); v != nil {
		t.Errorf("clock-tolerating spec should produce no violation; got %v", v)
	}
}

func TestFirstStrictViolation_TolerateViaService(t *testing.T) {
	rt := New(testLogger())
	src := `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = ["rand"],
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	rt.detectUnmediated("api", engine.SyscallEvent{Syscall: "getrandom", PID: 1234})

	if v := rt.firstStrictViolation(rt.events.Events()); v != nil {
		t.Errorf("per-service tolerance should suppress the violation; got %v", v)
	}
}

func TestFirstStrictViolation_PerServiceScoped(t *testing.T) {
	// Tolerance on service A must not silence the same category on service B.
	rt := New(testLogger())
	src := `
service("api", "/bin/true",
    interface("main", "http", 8080),
    nondeterministic_ok = ["clock"],
)
service("worker", "/bin/true",
    interface("main", "http", 8081),
)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	rt.detectUnmediated("worker", engine.SyscallEvent{Syscall: "clock_gettime", PID: 1234})

	v := rt.firstStrictViolation(rt.events.Events())
	if v == nil {
		t.Fatal("worker should still trip; api's tolerance must not leak")
	}
	if v.Service != "worker" {
		t.Errorf("violation.Service = %q; want worker", v.Service)
	}
}

func TestStrictViolationReason_NamesEverything(t *testing.T) {
	ev := &Event{
		Service: "api",
		Type:    "unmediated_io",
		Fields: map[string]string{
			"category": "dns",
			"syscall":  "connect",
			"pid":      "4242",
			"detail":   "8.8.8.8:53",
		},
	}
	got := strictViolationReason(ev)
	for _, want := range []string{"dns", "api", "connect", "4242", "8.8.8.8:53", "determinism(allow=", "nondeterministic_ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason missing %q; got %q", want, got)
		}
	}
}
