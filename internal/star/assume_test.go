package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestAssume_TopLevelTrueIsNoop(t *testing.T) {
	rt := New(testLogger())
	src := `assume(True)
assume(lambda choices: True)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if len(rt.specAssumes) != 2 {
		t.Errorf("expected 2 registered assumes, got %d", len(rt.specAssumes))
	}
}

func TestAssume_TopLevelFalseFailsSpecLoad(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `assume(False)`)
	if err == nil {
		t.Fatal("assume(False) at spec load must error")
	}
	if !strings.Contains(err.Error(), "violated") {
		t.Errorf("error should mention violation; got %v", err)
	}
}

func TestAssume_LambdaInspectsChoices(t *testing.T) {
	rt := New(testLogger())
	// retries selects first option = 0; predicate "retries == 0" → True.
	src := `
retries = choose("retries", [0, 1, 3])
assume(lambda choices: choices["retries"] == 0)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
}

func TestAssume_LambdaFalseFailsAtLoad(t *testing.T) {
	rt := New(testLogger())
	// retries first option = 0; predicate "retries > 5" → False.
	err := rt.LoadString("spec.star", `
retries = choose("retries", [0, 1, 3])
assume(lambda choices: choices["retries"] > 5)
`)
	if err == nil {
		t.Fatal("predicate that does not hold must error")
	}
}

func TestAssume_RejectsBadArgTypes(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("a.star", `assume("not a predicate")`); err == nil {
		t.Error("string predicate must error")
	}
	if err := rt.LoadString("b.star", `assume()`); err == nil {
		t.Error("zero-arg assume() must error")
	}
}

func TestAssume_PerTestHaltsWhenFalse(t *testing.T) {
	rt := New(testLogger())
	src := `
def body():
    return None

test("guarded", body=body, assume=[lambda choices: False])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	// DiscoverTests registers the body into globals.
	_ = rt.DiscoverTests()
	tr := rt.RunTest(context.Background(), "test_guarded")
	if tr.Result != "halted" {
		t.Errorf("Result = %q, want halted", tr.Result)
	}
}

func TestAssume_PerTestPassesWhenTrue(t *testing.T) {
	rt := New(testLogger())
	src := `
def body():
    return None

test("ok", body=body, assume=[lambda choices: True])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	_ = rt.DiscoverTests()
	tr := rt.RunTest(context.Background(), "test_ok")
	if tr.Result != "pass" {
		t.Errorf("Result = %q, want pass; reason=%s", tr.Result, tr.Reason)
	}
}

func TestAssume_PredicateInternalError(t *testing.T) {
	rt := New(testLogger())
	src := `
def body():
    return None

# Predicate indexes a missing dict key — evaluation raises at runtime.
test("raises", body=body, assume=[lambda choices: choices["missing_key"] == 0])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	_ = rt.DiscoverTests()
	tr := rt.RunTest(context.Background(), "test_raises")
	// RFC-043 Q3 (PR #118 follow-up): predicate Starlark errors map
	// to "error", not "fail". A predicate-authoring bug is
	// morphologically a runtime crash, not a behavioral failure of
	// the SUT.
	if tr.Result != "error" {
		t.Errorf("Result = %q, want error (predicate raised); reason=%s", tr.Result, tr.Reason)
	}
}

// TestAssume_PerLeafChoicesVisible — RFC-043 §5.4 rc2: per-test
// assume= predicates see the *current leaf's* axis values, not the
// discovery-run defaults. A predicate that filters on
// choices["retries"] < 5 trims the 9-leaf cross-product down to the
// surviving subset.
func TestAssume_PerLeafChoicesVisible(t *testing.T) {
	rt := New(testLogger())
	src := `
def body():
    _ = choose("retries", [1, 7])
    _ = choose("backoff", [10, 100])

# The predicate halts any leaf where retries == 7.
test("filtered", body=body, assume=[lambda choices: choices["retries"] != 7])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	// 4 total leaves (2 × 2). Half should be halted (retries == 7),
	// half should pass.
	if res.Pass+res.Halted != 4 {
		for _, tr := range res.Tests {
			t.Logf("  leaf %s: result=%s reason=%s", tr.LeafID, tr.Result, tr.Reason)
		}
		t.Errorf("expected 4 total verdicts, got pass=%d halted=%d fail=%d (%d rows)",
			res.Pass, res.Halted, res.Fail, len(res.Tests))
	}
	if res.Halted != 2 {
		t.Errorf("expected 2 halted (retries=7 leaves), got %d", res.Halted)
	}
	if res.Pass != 2 {
		t.Errorf("expected 2 pass (retries=1 leaves), got %d", res.Pass)
	}
}

// TestAssume_SandboxRejectsFaultCall — RFC-043 §8.7: assume()
// predicates referencing fault() (or any other runtime-mutating
// builtin) are rejected at spec load. Same model as the monitor
// sandbox.
func TestAssume_SandboxRejectsFaultCall(t *testing.T) {
	rt := New(testLogger())
	src := `
def body(): pass
test("bad", body=body, assume=[lambda c: fault])
`
	err := rt.LoadString("spec.star", src)
	if err == nil {
		t.Fatal("expected spec-load error for assume predicate referencing fault()")
	}
	if !strings.Contains(err.Error(), "fault") || !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("error should call out disallowed fault reference; got: %v", err)
	}
}

// TestAssume_SandboxRejectsTopLevelAssumeWithDeniedBuiltin —
// top-level `assume(...)` predicates go through the same sandbox.
func TestAssume_SandboxRejectsTopLevelAssumeWithDeniedBuiltin(t *testing.T) {
	rt := New(testLogger())
	src := `assume(lambda c: parallel)`
	err := rt.LoadString("spec.star", src)
	if err == nil {
		t.Fatal("expected spec-load error for top-level assume referencing parallel")
	}
	if !strings.Contains(err.Error(), "parallel") {
		t.Errorf("error should call out parallel; got: %v", err)
	}
}

// TestAssume_SandboxAllowsChoicesDictReads — predicates can read
// the `choices` argument freely; only Faultbox builtins are denied.
func TestAssume_SandboxAllowsChoicesDictReads(t *testing.T) {
	rt := New(testLogger())
	src := `
def body(): pass
test("ok", body=body, assume=[lambda c: c.get("anything", 0) == 0])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString should succeed; got %v", err)
	}
}

func TestCurrentChoicesDict_SkipsUnnamed(t *testing.T) {
	rt := New(testLogger())
	rt.recordChoice(&ChoiceVal{Name: "named", Options: []starlark.Value{starlark.MakeInt(42)}})
	rt.recordChoice(&ChoiceVal{Options: []starlark.Value{starlark.MakeInt(99)}}) // unnamed
	d := rt.currentChoicesDict()
	if d.Len() != 1 {
		t.Errorf("dict size = %d, want 1 (unnamed choice should be skipped)", d.Len())
	}
	v, found, _ := d.Get(starlark.String("named"))
	if !found || v != starlark.MakeInt(42) {
		t.Errorf("named key missing or wrong; got found=%v v=%v", found, v)
	}
}
