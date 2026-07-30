package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// RFC-052 Gap 8 — NO_POSITIVE_CONTROL.
//
// The case that motivates all of this: `testops/corpus/postgres_fault_basic.star`
// exercised a broken Postgres client in CI on every pull request from before
// v0.15.0 until v0.16.0, and passed. It set POSTGRES_HOST_AUTH_METHOD=trust
// (removing the credential path) and asserted the query *fails* under an
// injected fault — which a client that cannot connect at all satisfies
// identically.
//
// The gate for this milestone is a known-bad / known-good pair: fire on that
// shape, stay silent on the poc/protocol-audit shape. A version that fails
// either half is wrong, and the second half matters more — a diagnostic that
// fires on correct suites gets muted, and a muted diagnostic is worse than none.

// pg is the interface under test in all of these.
var pgMain = ifaceRef{Service: "pg", Interface: "main"}

// The known-bad shape: the only step on the interface fails, and the test
// asserts that it fails. Nothing establishes the client works.
func TestNoPositiveControl_FiresWhenOnlyFailureIsAsserted(t *testing.T) {
	v := newVacuityState()

	v.noteStep("pg", "main", false) // resp = pg.main.query(...) → failed
	v.noteAssertion("test_proxy_fault_rewrites_select")
	v.endTest("test_proxy_fault_rewrites_select", "pass")

	diags := v.suiteDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.Code != "NO_POSITIVE_CONTROL" {
		t.Errorf("code = %q", d.Code)
	}
	if d.Level != "warning" {
		t.Errorf("level = %q, want warning — a negative-only suite is a smell, not an error", d.Level)
	}
	if !strings.Contains(d.Message, "pg.main") {
		t.Errorf("message should name the interface: %q", d.Message)
	}
	// The suggestion is the agent's next move; it has to say what to do.
	for _, want := range []string{"cannot", "connect", "r.ok"} {
		if !strings.Contains(d.Suggestion, want) {
			t.Errorf("suggestion should mention %q: %q", want, d.Suggestion)
		}
	}
}

// The known-good shape: poc/protocol-audit pairs a step that must succeed with
// one that must fail. That pairing is the entire reason those specs found the
// credential bugs, and it must not be flagged.
func TestNoPositiveControl_SilentWhenSuccessIsAsserted(t *testing.T) {
	v := newVacuityState()

	// test_exec_and_query: CREATE TABLE + INSERT + SELECT all succeed, asserted.
	v.noteStep("pg", "main", true)
	v.noteAssertion("test_exec_and_query")
	v.endTest("test_exec_and_query", "pass")

	// test_error_is_reported_not_swallowed: a bad statement must fail, asserted.
	v.noteStep("pg", "main", false)
	v.noteAssertion("test_error_is_reported_not_swallowed")
	v.endTest("test_error_is_reported_not_swallowed", "pass")

	if diags := v.suiteDiagnostics(); len(diags) != 0 {
		t.Fatalf("a suite with both a positive and a negative control must be silent, got: %+v", diags)
	}
}

// A single fault-injection test asserting failure is correct on its own. The
// diagnostic is about the *suite*, so the order tests run in cannot matter.
func TestNoPositiveControl_OrderIndependent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		v := newVacuityState()
		steps := []struct {
			ok   bool
			test string
		}{
			{false, "test_fault"},
			{true, "test_happy"},
		}
		if reverse {
			steps[0], steps[1] = steps[1], steps[0]
		}
		for _, s := range steps {
			v.noteStep("pg", "main", s.ok)
			v.noteAssertion(s.test)
			v.endTest(s.test, "pass")
		}
		if diags := v.suiteDiagnostics(); len(diags) != 0 {
			t.Errorf("reverse=%v: expected silence, got %+v", reverse, diags)
		}
	}
}

// A vacuous test must not supply the positive control. The step result proves
// the client works, but if nothing asserts on it the suite would not fail when
// it breaks — which is the whole property being checked.
func TestNoPositiveControl_VacuousTestCannotSupplyIt(t *testing.T) {
	v := newVacuityState()

	v.noteStep("pg", "main", true) // succeeded...
	v.endTest("test_prints_only", "pass")

	diags := v.suiteDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("a passing test with zero assertions must not count as a positive control, got %+v", diags)
	}
}

// A failing test must not supply it either: its own verdict is unreliable
// evidence about anything.
func TestNoPositiveControl_FailingTestCannotSupplyIt(t *testing.T) {
	v := newVacuityState()
	v.noteStep("pg", "main", true)
	v.noteAssertion("test_broken")
	v.endTest("test_broken", "fail")

	if diags := v.suiteDiagnostics(); len(diags) != 1 {
		t.Fatalf("a failing test must not supply the positive control, got %+v", diags)
	}
}

// Candidates must not leak across tests. Otherwise a success in test A followed
// by a vacuous test B would let B confirm A's candidate, or vice versa — the
// per-test state bug that bit RFC-056's guards.
func TestNoPositiveControl_CandidatesDoNotLeakBetweenTests(t *testing.T) {
	v := newVacuityState()

	// Test A: step succeeds, but A asserts nothing → no control.
	v.noteStep("pg", "main", true)
	v.endTest("test_a", "pass")

	// Test B: asserts, but its own step fails → still no control.
	v.noteStep("pg", "main", false)
	v.noteAssertion("test_b")
	v.endTest("test_b", "pass")

	if diags := v.suiteDiagnostics(); len(diags) != 1 {
		t.Fatalf("B's assertion must not confirm A's leaked candidate, got %+v", diags)
	}
}

// Interfaces are tracked independently: proving Redis works says nothing about
// Postgres.
func TestNoPositiveControl_PerInterface(t *testing.T) {
	v := newVacuityState()

	v.noteStep("cache", "main", true)
	v.noteStep("pg", "main", false)
	v.noteAssertion("test_mixed")
	v.endTest("test_mixed", "pass")

	diags := v.suiteDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("expected only pg.main flagged, got %+v", diags)
	}
	if diags[0].Service != "pg" {
		t.Errorf("flagged service = %q, want pg", diags[0].Service)
	}
}

// An interface never stepped is not flagged — the diagnostic is about
// interfaces the suite claims to exercise, not every declared one. A spec that
// declares a Redis it never touches has a coverage gap, which is a different
// finding with a different remedy.
func TestNoPositiveControl_SilentForUnsteppedInterfaces(t *testing.T) {
	v := newVacuityState()
	v.noteAssertion("test_something")
	v.endTest("test_something", "pass")

	if diags := v.suiteDiagnostics(); len(diags) != 0 {
		t.Fatalf("an interface never stepped must not be flagged, got %+v", diags)
	}
}

// Output must be deterministic — bundles and goldens are byte-compared.
func TestNoPositiveControl_DeterministicOrder(t *testing.T) {
	build := func() []string {
		v := newVacuityState()
		for _, svc := range []string{"zeta", "alpha", "mid"} {
			v.noteStep(svc, "main", false)
		}
		v.noteAssertion("t")
		v.endTest("t", "pass")
		var out []string
		for _, d := range v.suiteDiagnostics() {
			out = append(out, d.Service)
		}
		return out
	}
	first := build()
	if len(first) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d", len(first))
	}
	if first[0] != "alpha" || first[1] != "mid" || first[2] != "zeta" {
		t.Errorf("not sorted by service: %v", first)
	}
	for i := 0; i < 20; i++ {
		if got := build(); !equalStrings(got, first) {
			t.Fatalf("order varied between runs: %v vs %v", got, first)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Assertions outside a test (top-level, setup) are not attributable and must
// not be counted toward any test.
func TestAssertionsOutsideATestAreNotCounted(t *testing.T) {
	v := newVacuityState()
	v.noteAssertion("")
	if got := v.assertionCount(""); got != 0 {
		t.Errorf("unattributed assertion counted: %d", got)
	}
}

// Every name in assertionBuiltins must actually be a registered builtin.
// Without this, a rename silently stops counting that assertion and every test
// using it starts looking vacuous — a false positive, which is the failure mode
// that would discredit the diagnostic.
func TestAssertionBuiltinsAllExist(t *testing.T) {
	rt := New(testLogger())
	b := rt.builtins()
	for name := range assertionBuiltins {
		v, ok := b[name]
		if !ok {
			t.Errorf("assertionBuiltins lists %q, which is not a registered builtin — "+
				"a rename would make tests using it look vacuous", name)
			continue
		}
		if _, ok := v.(*starlark.Builtin); !ok {
			t.Errorf("%q is registered as %T, not *starlark.Builtin; the counter wrapper would be skipped", name, v)
		}
	}
}

// Wrapping must preserve the builtin's name, or spec-facing error messages and
// tracebacks change.
func TestAssertionWrapperPreservesName(t *testing.T) {
	rt := New(testLogger())
	b := rt.builtins()
	for name := range assertionBuiltins {
		if bi, ok := b[name].(*starlark.Builtin); ok && bi.Name() != name {
			t.Errorf("wrapped builtin reports name %q, want %q", bi.Name(), name)
		}
	}
}

// The counter must actually fire through the wrapper — the wiring, not just the
// bookkeeping.
func TestAssertionCounterIncrementsThroughSpec(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_asserts():
    assert_true(True, "ok")
    assert_eq(1, 1)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	rt.currentTestName = "test_asserts"
	fn, ok := rt.globals["test_asserts"].(starlark.Callable)
	if !ok {
		t.Fatal("test_asserts not callable")
	}
	thread := &starlark.Thread{Name: "test"}
	if _, err := starlark.Call(thread, fn, nil, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := rt.vacuity.assertionCount("test_asserts"); got != 2 {
		t.Errorf("assertion count = %d, want 2 — the registration wrapper is not firing", got)
	}
}

// FAULT_FIRED_BUT_SUCCESS was miscalibrated from v0.12 to v0.17.0, and nobody
// noticed because per-test diagnostics were never printed. Its heuristic —
// a fault fired and the test passed — describes the single most common correct
// shape in the tool: inject a fault, assert the service degrades gracefully.
//
// It now requires the test to have asserted nothing. With assertions present
// the author checked the behaviour; with none they checked nothing, and that is
// worth saying.
func TestFaultFiredButSuccessRequiresNoAssertions(t *testing.T) {
	faults := []FaultInfo{{Service: "api", Syscall: "connect", Action: "deny", Hits: 1}}

	// The correct shape: fault fired, test passed, author asserted. Silent.
	asserted := buildDiagnostics(
		&TestTraceOutput{Faults: faults},
		&TestResult{Name: "t", Result: "pass", Assertions: 2},
	)
	for _, d := range asserted {
		if d.Code == "FAULT_FIRED_BUT_SUCCESS" {
			t.Errorf("fired on a test that asserted %d times — this is the shape "+
				"`inject a fault, assert graceful degradation`, which is correct", 2)
		}
	}

	// Nothing asserted: the diagnostic is the whole value.
	bare := buildDiagnostics(
		&TestTraceOutput{Faults: faults},
		&TestResult{Name: "t", Result: "pass", Assertions: 0},
	)
	var found bool
	for _, d := range bare {
		if d.Code == "FAULT_FIRED_BUT_SUCCESS" {
			found = true
		}
	}
	if !found {
		t.Errorf("did not fire on a fault that hit a test asserting nothing: %+v", bare)
	}
}
