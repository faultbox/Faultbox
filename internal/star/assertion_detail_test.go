package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestAssertEqPopulatesAssertionDetail verifies that a failing
// assert_eq() captures Expected/Actual on the runtime so RunTest can
// snapshot it into TestResult.Assertion. Without this, the report
// drill-down has nothing structured to render and falls back to the
// free-form Reason string — exactly the case Boris flagged on the
// regenerated v0.12 report ("order feed should work" with no values).
func TestAssertEqPopulatesAssertionDetail(t *testing.T) {
	rt := &Runtime{}
	thread := &starlark.Thread{Name: "test"}
	args := starlark.Tuple{starlark.MakeInt(500), starlark.MakeInt(200), starlark.String("order feed should work")}
	_, err := rt.builtinAssertEq(thread, nil, args, nil)
	if err == nil {
		t.Fatal("assert_eq should have failed on 500 != 200")
	}
	if rt.lastAssertion == nil {
		t.Fatal("lastAssertion should be populated after a failing assert_eq")
	}
	got := rt.lastAssertion
	if got.Func != "assert_eq" {
		t.Errorf("Func: got %q, want assert_eq", got.Func)
	}
	if got.Actual != "500" {
		t.Errorf("Actual: got %q, want 500", got.Actual)
	}
	if got.Expected != "200" {
		t.Errorf("Expected: got %q, want 200", got.Expected)
	}
	if got.Message != "order feed should work" {
		t.Errorf("Message: got %q, want order feed should work", got.Message)
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "200") {
		t.Errorf("err string should still carry both values: %v", err)
	}
}

// TestAssertTruePopulatesAssertionDetail covers the assert_true path
// — Expected is always "True", Actual is the Starlark printable form
// of the user-supplied value.
func TestAssertTruePopulatesAssertionDetail(t *testing.T) {
	rt := &Runtime{}
	thread := &starlark.Thread{Name: "test"}
	args := starlark.Tuple{starlark.MakeInt(0), starlark.String("nonzero result expected")}
	_, err := rt.builtinAssertTrue(thread, nil, args, nil)
	if err == nil {
		t.Fatal("assert_true should have failed on falsy value 0")
	}
	if rt.lastAssertion == nil {
		t.Fatal("lastAssertion should be populated after a failing assert_true")
	}
	got := rt.lastAssertion
	if got.Func != "assert_true" {
		t.Errorf("Func: got %q, want assert_true", got.Func)
	}
	if got.Expected != "True" {
		t.Errorf("Expected: got %q, want True", got.Expected)
	}
	if got.Actual != "0" {
		t.Errorf("Actual: got %q, want 0", got.Actual)
	}
}

// TestAssertCapturesCallerPosition runs a real Starlark program so
// the runtime threads accumulate a callstack — the source-line capture
// (v0.12.3) only works when the assert builtin can read its caller's
// frame, and a unit test that calls the builtin directly bypasses
// that path. This is the regression test for the "Expression: …"
// row in the report drill-down.
func TestAssertCapturesCallerPosition(t *testing.T) {
	rt := &Runtime{}
	predeclared := starlark.StringDict{
		"assert_true": starlark.NewBuiltin("assert_true", rt.builtinAssertTrue),
		"assert_eq":   starlark.NewBuiltin("assert_eq", rt.builtinAssertEq),
	}
	src := "" +
		"def go():\n" +
		"    x = 5\n" +
		"    assert_true(x == 7, \"x should be 7\")\n" +
		"go()\n"
	thread := &starlark.Thread{Name: "test"}
	_, err := starlark.ExecFile(thread, "spec.star", src, predeclared)
	if err == nil {
		t.Fatal("ExecFile should fail because assert_true raised")
	}
	if rt.lastAssertion == nil {
		t.Fatal("expected lastAssertion to be populated")
	}
	got := rt.lastAssertion
	if got.File != "spec.star" {
		t.Errorf("File: got %q, want spec.star", got.File)
	}
	if got.Line != 3 {
		t.Errorf("Line: got %d, want 3 (assert_true line in src)", got.Line)
	}
}

// TestAssertCapturesRecentContext verifies the v0.12.4 Context
// snapshot — when a `assert_true(resp.status in [200, 201])` fires,
// the most recent step_recv event (with status_code on it) lands on
// AssertionDetail.Context so the report can show "Recent: ←
// api.http.post /orders [500]" without the user pinning the lane.
func TestAssertCapturesRecentContext(t *testing.T) {
	rt := &Runtime{events: NewEventLog()}
	rt.events.Emit("step_send", "test", map[string]string{
		"target": "api", "method": "post", "summary": "→ api.post /orders",
	})
	rt.events.Emit("step_recv", "test", map[string]string{
		"target": "api", "method": "post", "summary": "← api.post /orders [500]",
		"status_code": "500", "success": "false", "error": "internal error",
	})
	thread := &starlark.Thread{Name: "test"}
	args := starlark.Tuple{starlark.False, starlark.String("status should be 200")}
	if _, err := rt.builtinAssertTrue(thread, nil, args, nil); err == nil {
		t.Fatal("assert_true should fail on a False condition")
	}
	if rt.lastAssertion == nil || len(rt.lastAssertion.Context) == 0 {
		t.Fatal("Context should carry the recent step events")
	}
	got := rt.lastAssertion.Context
	last := got[len(got)-1]
	if last.Type != "step_recv" {
		t.Errorf("last context entry type: got %q, want step_recv", last.Type)
	}
	if last.StatusCode != "500" {
		t.Errorf("StatusCode: got %q, want 500", last.StatusCode)
	}
	if last.Success != "false" {
		t.Errorf("Success: got %q, want false", last.Success)
	}
	if !strings.Contains(last.Error, "internal error") {
		t.Errorf("Error: got %q, expected to contain 'internal error'", last.Error)
	}
}

// TestAssertEqPassDoesNotPopulate guarantees we don't leave stale
// AssertionDetail behind on a passing call — the runtime resets
// lastAssertion at the top of RunTest, but a successful assert_eq in
// the middle of a test must not overwrite a prior failure.
func TestAssertEqPassDoesNotPopulate(t *testing.T) {
	rt := &Runtime{}
	thread := &starlark.Thread{Name: "test"}
	args := starlark.Tuple{starlark.MakeInt(7), starlark.MakeInt(7)}
	if _, err := rt.builtinAssertEq(thread, nil, args, nil); err != nil {
		t.Fatalf("assert_eq should pass on equal values: %v", err)
	}
	if rt.lastAssertion != nil {
		t.Errorf("lastAssertion should remain nil on pass, got %+v", rt.lastAssertion)
	}
}
