package star

import (
	"testing"

	"go.starlark.net/starlark"
)

// TestCallerInTestDetectsTestFrame verifies the v0.12.10 spec-anchor
// detection: a call from inside `def test_foo()` returns the test
// name, while a call from a non-test scope returns "". Used by
// executeStep to tag step_send / step_recv events with `fields.spec`
// so the report renderer can highlight user-written calls against a
// sea of monitor / proxy / background traffic.
func TestCallerInTestDetectsTestFrame(t *testing.T) {
	rt := &Runtime{currentTestName: "test_order_creation"}
	captured := ""
	probe := starlark.NewBuiltin("probe", func(thread *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		captured = rt.callerInTest(thread)
		return starlark.None, nil
	})
	predeclared := starlark.StringDict{"probe": probe}

	// Call from test_order_creation → must return the test name.
	src := "" +
		"def test_order_creation():\n" +
		"    probe()\n" +
		"test_order_creation()\n"
	thread := &starlark.Thread{Name: "test"}
	if _, err := starlark.ExecFile(thread, "spec.star", src, predeclared); err != nil {
		t.Fatalf("ExecFile: %v", err)
	}
	if captured != "test_order_creation" {
		t.Errorf("expected %q, got %q", "test_order_creation", captured)
	}

	// Call from an unrelated function → must return empty (this is
	// how monitor evaluations and recipe internals get correctly
	// excluded from the spec-anchor highlight).
	captured = ""
	src2 := "" +
		"def helper():\n" +
		"    probe()\n" +
		"helper()\n"
	thread2 := &starlark.Thread{Name: "test"}
	if _, err := starlark.ExecFile(thread2, "spec.star", src2, predeclared); err != nil {
		t.Fatalf("ExecFile: %v", err)
	}
	if captured != "" {
		t.Errorf("expected empty (no test frame on stack), got %q", captured)
	}
}

// TestCallerInTestThroughHelper guards the user-written-helper path:
// a call from `def post_order(): api.http.post(...)` inside
// `def test_X(): post_order()` should still register as
// spec-anchored — the test_X frame is on the stack, just not at the
// top.
func TestCallerInTestThroughHelper(t *testing.T) {
	rt := &Runtime{currentTestName: "test_outer"}
	captured := ""
	probe := starlark.NewBuiltin("probe", func(thread *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		captured = rt.callerInTest(thread)
		return starlark.None, nil
	})
	predeclared := starlark.StringDict{"probe": probe}

	src := "" +
		"def helper():\n" +
		"    probe()\n" +
		"def test_outer():\n" +
		"    helper()\n" +
		"test_outer()\n"
	thread := &starlark.Thread{Name: "test"}
	if _, err := starlark.ExecFile(thread, "spec.star", src, predeclared); err != nil {
		t.Fatalf("ExecFile: %v", err)
	}
	if captured != "test_outer" {
		t.Errorf("expected test_outer in stack to be detected, got %q", captured)
	}
}

// TestCallerInTestWithoutCurrentTestName — when the runtime isn't
// running a test (e.g. a service starting outside RunTest), no
// detection should fire.
func TestCallerInTestWithoutCurrentTestName(t *testing.T) {
	rt := &Runtime{currentTestName: ""}
	captured := "preset"
	probe := starlark.NewBuiltin("probe", func(thread *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		captured = rt.callerInTest(thread)
		return starlark.None, nil
	})
	predeclared := starlark.StringDict{"probe": probe}

	src := "" +
		"def test_x():\n" +
		"    probe()\n" +
		"test_x()\n"
	thread := &starlark.Thread{Name: "test"}
	if _, err := starlark.ExecFile(thread, "spec.star", src, predeclared); err != nil {
		t.Fatalf("ExecFile: %v", err)
	}
	if captured != "" {
		t.Errorf("expected empty when currentTestName is unset, got %q", captured)
	}
}
