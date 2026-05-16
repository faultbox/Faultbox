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
	if tr.Result != "fail" {
		t.Errorf("Result = %q, want fail (predicate raised); reason=%s", tr.Result, tr.Reason)
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
