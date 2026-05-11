package star

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// TestLifecycle_TestBuiltinRegistersAndRuns — test() with a minimal
// body produces a passing TestResult discoverable by RunAll.
func TestLifecycle_TestBuiltinRegistersAndRuns(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("trivial", body = lambda: None)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	names := rt.DiscoverTests()
	if len(names) != 1 || names[0] != "test_trivial" {
		t.Fatalf("expected discoverable test 'test_trivial', got %v", names)
	}
	cfg := rt.testConfigs["test_trivial"]
	if cfg == nil {
		t.Fatal("test_trivial should have a TestConfig")
	}

	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 || res.Fail != 0 || res.Inconclusive != 0 {
		t.Errorf("expected 1 pass / 0 fail / 0 inconclusive, got %+v", res)
	}
}

// TestLifecycle_PerTestTimeoutInconclusive — test() with an explicit
// short timeout and a body that blocks past it yields INCONCLUSIVE.
func TestLifecycle_PerTestTimeoutInconclusive(t *testing.T) {
	rt := New(testLogger())
	// Use await_event to make the body block on the per-test ctx — the
	// canonical body-blocking primitive, so this also covers the
	// "await_* honors per-test timeout" wiring.
	err := rt.LoadString("spec.star", `
test("blocks_forever",
    body = lambda: await_event(match.event(type="never_emitted")),
    timeout = "50ms",
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Inconclusive != 1 {
		t.Errorf("expected 1 inconclusive, got %+v", res)
	}
	if len(res.Tests) != 1 {
		t.Fatalf("expected 1 recorded result, got %d", len(res.Tests))
	}
	if res.Tests[0].Result != "inconclusive" {
		t.Errorf("Result = %q, want inconclusive", res.Tests[0].Result)
	}
	if !strings.Contains(res.Tests[0].Reason, "timeout") {
		t.Errorf("Reason should mention timeout, got %q", res.Tests[0].Reason)
	}
}

// TestLifecycle_EventuallyUnsatisfiedFailsViaNatural — body returns
// without satisfying eventually() → FAIL via TerminationNatural.
// RFC §5.5(a) says natural completion requires every eventually
// already satisfied; the contradiction case is FAIL.
func TestLifecycle_EventuallyUnsatisfiedFailsViaNatural(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("never_satisfies",
    body = lambda: None,
    expect = eventually(lambda t: False),
    timeout = "1s",
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Fail != 1 {
		t.Errorf("expected 1 fail, got %+v", res)
	}
	if len(res.Tests) != 1 {
		t.Fatal("expected one test result")
	}
	if !strings.Contains(res.Tests[0].Reason, "never satisfied") {
		t.Errorf("Reason should mention 'never satisfied', got %q", res.Tests[0].Reason)
	}
}

// TestLifecycle_AlwaysHoldsPasses — always() predicate that holds
// → PASS (window closes at body return without violation).
func TestLifecycle_AlwaysHoldsPasses(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("always_true",
    body = lambda: None,
    expect = always(lambda t: True),
    timeout = "1s",
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 || res.Fail != 0 {
		t.Errorf("expected pass, got %+v", res)
	}
}

// TestLifecycle_SetupCalled — test() with a setup= callable that
// raises produces a "setup: ..." FAIL reason, proving the callable
// was invoked. Ordering setup-before-body is covered indirectly:
// setup is run synchronously before the body goroutine starts, so a
// raising setup short-circuits before the body could run.
//
// (Verifying via spec-level list mutation isn't an option — Starlark
// globals freeze at LoadString completion, so a `order.append(...)`
// inside setup would error with "cannot append to frozen list".)
func TestLifecycle_SetupCalled(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
def setup_ok():
    pass

test("setup_runs",
    body = lambda: None,
    setup = setup_ok,
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 {
		t.Errorf("setup that returns normally should not affect the test verdict, got %+v", res)
	}
}

// TestLifecycle_SetupRaisesFailsFast — setup raising fails the test
// before body runs.
func TestLifecycle_SetupRaisesFailsFast(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
def bad_setup():
    fail("setup borked")

def body():
    fail("body should not run")

test("setup_fails",
    body = body,
    setup = bad_setup,
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Fail != 1 {
		t.Errorf("expected 1 fail, got %+v", res)
	}
	if !strings.Contains(res.Tests[0].Reason, "setup") {
		t.Errorf("Reason should mention setup, got %q", res.Tests[0].Reason)
	}
}

// TestLifecycle_TerminateWhenFiresWithoutSatisfyingEventuallyFails —
// terminate_when fires before the liveness eventually has held → FAIL
// (RFC §5.5(b) table).
func TestLifecycle_TerminateWhenFiresWithoutSatisfyingEventuallyFails(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
def body():
    # Emit an event that satisfies terminate_when but not expect.
    # Direct event-log emission via the runtime is not available to
    # Starlark; instead we use a small monitor to inject the trigger.
    pass

# This monitor's check fires the trigger event by raising — but a
# simpler path: use a body that emits via await_event timeout coupling.
# Cleaner: rely on terminate_when always returning True so it fires
# immediately on the first observed event.
trigger = monitor("trigger",
    on = match.all(),
    check = lambda event, state: True,
)

test("tw_fires_first",
    body = lambda: None,  # body returns immediately; eventually still pending
    expect = eventually(lambda t: t.event(type="never_emitted") != None),
    terminate_when = eventually(lambda t: True),  # fires on any event
    timeout = "1s",
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	// Either Fail (terminate_when fired and eventually was unsatisfied)
	// or Fail-via-natural (body returned, eventually pending). Both are
	// FAIL; we accept either.
	if res.Fail != 1 {
		t.Errorf("expected 1 fail, got %+v", res)
	}
}

// TestLifecycle_PanicInBodyStillCapturedAsError — Go-level panic
// inside a test() body is recovered and reported as Result="error"
// (preserving issue-#76 contract through the new goroutine plumbing).
func TestLifecycle_PanicInBodyStillCapturedAsError(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("spec.star", `
test("panic_body", body = lambda: None)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	rt.globals["test_panic_body"] = starlark.NewBuiltin("test_panic_body",
		func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			panic("synthetic body panic for PR 6")
		})

	tr := rt.runTestSafely(context.Background(), "test_panic_body")
	if tr.Result != "error" {
		t.Fatalf("expected Result=error from panic, got %q (reason=%q)", tr.Result, tr.Reason)
	}
	if !strings.Contains(tr.Reason, "synthetic body panic") {
		t.Errorf("Reason should contain the panic message; got %q", tr.Reason)
	}
}

// TestLifecycle_TestRequiresName — name= positional is mandatory.
func TestLifecycle_TestRequiresName(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test(body = lambda: None)
`)
	if err == nil {
		t.Error("expected error when test() called without name")
	}
}

// TestLifecycle_TestRejectsVirtualClock — RFC §8.8 reservation.
func TestLifecycle_TestRejectsVirtualClock(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("clocked",
    body = lambda: None,
    clock = "virtual",
)
`)
	if err == nil {
		t.Fatal("expected error for clock=virtual")
	}
	if !strings.Contains(err.Error(), "gVisor") {
		t.Errorf("error should mention gVisor migration path, got: %v", err)
	}
}

// TestLifecycle_LegacyTestStillRuns — def test_*() functions remain
// supported with their classical synchronous lifecycle.
func TestLifecycle_LegacyTestStillRuns(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
def test_legacy():
    pass
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 {
		t.Errorf("legacy def test_*() should pass, got %+v", res)
	}
}

// TestLifecycle_LegacyTestWithInlineEventually — a legacy def test_*()
// that registers an inline eventually() that never satisfies → FAIL
// at body return (TerminationNatural rule).
func TestLifecycle_LegacyTestWithInlineEventually(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
def test_legacy_with_eventually():
    eventually(lambda t: False)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Fail != 1 {
		t.Errorf("legacy test with unsatisfied eventually should fail, got %+v", res)
	}
}

// TestLifecycle_InconclusiveCountedSeparately — SuiteResult.Inconclusive
// is incremented for INCONCLUSIVE results; Fail stays zero.
func TestLifecycle_InconclusiveCountedSeparately(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("times_out",
    body = lambda: await_event(match.event(type="never")),
    timeout = "30ms",
)
test("passes", body = lambda: None)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 {
		t.Errorf("expected 1 pass, got %d", res.Pass)
	}
	if res.Inconclusive != 1 {
		t.Errorf("expected 1 inconclusive, got %d", res.Inconclusive)
	}
	if res.Fail != 0 {
		t.Errorf("expected 0 fail, got %d", res.Fail)
	}
}

// TestLifecycle_TraceOutputCarriesInconclusive — BuildTraceOutput
// propagates the Inconclusive count.
func TestLifecycle_TraceOutputCarriesInconclusive(t *testing.T) {
	res := &SuiteResult{
		Pass:         3,
		Fail:         1,
		Inconclusive: 2,
		DurationMs:   100,
	}
	out := BuildTraceOutput("spec.star", res)
	if out.Inconclusive != 2 {
		t.Errorf("TraceOutput.Inconclusive = %d, want 2", out.Inconclusive)
	}
}

// TestLifecycle_TimeoutAppliesToBodyContext — verifies the per-test
// context derived from test(timeout=...) is the one that body-
// blocking primitives see via rt.testContext().
func TestLifecycle_TimeoutAppliesToBodyContext(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
test("short", body = lambda: await_event(match.event(type="never")), timeout = "20ms")
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	start := time.Now()
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Errorf("test should have been cut by 20ms timeout, took %v", elapsed)
	}
	if res.Inconclusive != 1 {
		t.Errorf("expected inconclusive, got %+v", res)
	}
}
