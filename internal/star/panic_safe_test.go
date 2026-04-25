package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestRunTestSafelyConvertsPanicToErrorResult exercises the recover
// wrapper introduced for issue #76: a Go runtime panic inside a test
// (or anything it transitively calls) must turn into an "error"
// TestResult instead of taking the whole suite — and the .fb bundle
// — down with it.
func TestRunTestSafelyConvertsPanicToErrorResult(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("panic.star", `
def test_passes():
    pass
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// Inject a synthetic test that panics immediately when invoked.
	// This stands in for a runtime panic anywhere down the call
	// stack (applyFaults, container start, etc.) without needing to
	// reach into those packages.
	rt.globals["test_panics"] = starlark.NewBuiltin("test_panics",
		func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			panic("synthetic test-runtime panic for #76")
		})

	tr := rt.runTestSafely(context.Background(), "test_panics")
	if tr.Result != "error" {
		t.Fatalf("expected Result=error, got %q (reason=%q)", tr.Result, tr.Reason)
	}
	if !strings.Contains(tr.Reason, "synthetic test-runtime panic") {
		t.Errorf("Reason should carry the panic message; got %q", tr.Reason)
	}
	if !strings.Contains(tr.Reason, "goroutine") {
		t.Errorf("Reason should carry a stack trace; got %q", tr.Reason)
	}

	// And — the headline #76 contract — the runtime must still be
	// usable after a recovered panic. The next test in the same
	// runtime must run cleanly.
	tr2 := rt.runTestSafely(context.Background(), "test_passes")
	if tr2.Result != "pass" {
		t.Errorf("post-panic test should still run; got Result=%q reason=%q",
			tr2.Result, tr2.Reason)
	}
}

// TestRunAllRecordsCrashInfoOnFirstPanic checks that when a panicking
// test does fire, SuiteResult.Crash is populated with the first crash
// (panic message + last test name). Downstream wires this into the
// bundle's manifest.crash field so users opening the report see a
// loud "this run is partial" signal — not a silent green dashboard.
func TestRunAllRecordsCrashInfoOnFirstPanic(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("crash.star", `
def test_a():
    pass
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	rt.globals["test_b_panics"] = starlark.NewBuiltin("test_b_panics",
		func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			panic("kaboom")
		})

	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Crash == nil {
		t.Fatal("expected SuiteResult.Crash to be populated after a panicking test")
	}
	if !strings.Contains(res.Crash.Panic, "kaboom") {
		t.Errorf("Crash.Panic should include the panic value; got %q", res.Crash.Panic)
	}
	if res.Crash.LastTest != "test_b_panics" {
		t.Errorf("Crash.LastTest = %q, want test_b_panics", res.Crash.LastTest)
	}
}
