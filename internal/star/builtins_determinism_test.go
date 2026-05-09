package star

import (
	"testing"
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
