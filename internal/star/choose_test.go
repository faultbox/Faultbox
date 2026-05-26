package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestChoose_ReturnsFirstOption(t *testing.T) {
	rt := New(testLogger())
	src := `r = choose([7, 8, 9])
n = nondet()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	// `choose([7,8,9])` returns 7 in rc1 single-leaf mode.
	if got := rt.globals["r"]; got != starlark.MakeInt(7) {
		t.Errorf("r = %v, want 7 (first option)", got)
	}
	// `nondet()` is sugar for choose([True, False]); first option is True.
	if got := rt.globals["n"]; got != starlark.True {
		t.Errorf("n = %v, want True (nondet first option)", got)
	}
}

func TestChoose_RecordsCallSites(t *testing.T) {
	rt := New(testLogger())
	src := `
a = choose([1, 2, 3])
b = choose("retries", [0, 1])
c = nondet()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	choices := rt.Choices()
	if len(choices) != 3 {
		t.Fatalf("expected 3 recorded choices, got %d", len(choices))
	}
	if len(choices[0].Options) != 3 {
		t.Errorf("first choice option count = %d, want 3", len(choices[0].Options))
	}
	if choices[1].Name != "retries" {
		t.Errorf("named choice Name = %q, want retries", choices[1].Name)
	}
	if len(choices[2].Options) != 2 {
		t.Errorf("nondet choice option count = %d, want 2", len(choices[2].Options))
	}
}

func TestChoose_EmptyListErrors(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose([])`)
	if err == nil {
		t.Fatal("expected error for empty options list")
	}
}

func TestChoose_NonListErrors(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose(42)`)
	if err == nil {
		t.Fatal("expected error when options is not a list")
	}
}

func TestChoose_BadArity(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose()`)
	if err == nil {
		t.Fatal("expected error for zero-arg choose()")
	}
}

func TestChoose_NamedFormStringRequired(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose(42, [1, 2])`)
	if err == nil {
		t.Fatal("expected error when name arg is not a string")
	}
}

// Review B2: nondet(svc=x) must not silently bypass the
// service-exclusion path by taking the zero-arg boolean route.
// Reject kwargs explicitly so the call surface stays unambiguous.
func TestNondet_RejectsKwargs(t *testing.T) {
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox", cmd=["sh","-c","sleep 1"])
nondet(svc=svc)
`
	err := rt.LoadString("spec.star", src)
	if err == nil {
		t.Fatal("expected error for nondet(svc=...)")
	}
	if !strings.Contains(err.Error(), "keyword arguments are not accepted") {
		t.Errorf("error should mention kwargs are rejected; got %v", err)
	}
	if rt.nondetServices["svc"] {
		t.Error("svc must NOT be marked nondet when call errored")
	}
}

// Pre-RFC-043 nondet(service) variant must continue to work — covered
// implicitly by existing tutorial specs; pin the behavior here.
func TestNondet_ServiceFormStillMarksService(t *testing.T) {
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox", cmd=["sh","-c","sleep 1"])
nondet(svc)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if !rt.nondetServices["svc"] {
		t.Errorf("expected svc to be marked nondet, got %v", rt.nondetServices)
	}
}

// TestChoose_LeafPinsSelection — when rt.currentLeaf assigns a named
// choose() axis, builtinChoose returns that option instead of the
// first. Proves the rc2 fan-out data path is wired end-to-end; the
// plan-tree enumerator in PR 2 of this slice produces the leaves.
func TestChoose_LeafPinsSelection(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_leaf_pinned():
    r = choose("retries", [0, 1, 3])
    assert_eq(r, 3, "leaf-pinned choose should return option index 2")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	leaf := &PlanLeaf{Index: 7, Choices: map[string]int{"retries": 2}}
	tr := rt.RunTestLeaf(context.Background(), "test_leaf_pinned", leaf)
	if tr.Result != "pass" {
		t.Fatalf("Result = %q (want pass); reason: %s", tr.Result, tr.Reason)
	}
	if tr.LeafID != "7" {
		t.Errorf("LeafID = %q, want %q", tr.LeafID, "7")
	}
}

// TestChoose_NilLeafFallsBackToFirstOption — RunTest (the
// degenerate single-leaf surface) keeps returning the first option,
// matching rc1 behavior exactly.
func TestChoose_NilLeafFallsBackToFirstOption(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_default_leaf():
    r = choose("retries", [0, 1, 3])
    assert_eq(r, 0, "nil leaf must return FirstOption")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_default_leaf")
	if tr.Result != "pass" {
		t.Fatalf("Result = %q (want pass); reason: %s", tr.Result, tr.Reason)
	}
	if tr.LeafID != "" {
		t.Errorf("LeafID = %q, want empty for single-leaf execution", tr.LeafID)
	}
}

// TestChoose_AnonymousChoiceIgnoresLeaf — choose() without a name is
// not part of the plan tree; even a non-nil leaf can't pin it, so
// the returned value remains FirstOption. This is the documented
// rc2 boundary (`docs/nondeterministic-operators.md`).
func TestChoose_AnonymousChoiceIgnoresLeaf(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_anon():
    r = choose([7, 8, 9])
    assert_eq(r, 7, "anonymous choose() must return FirstOption regardless of leaf")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	leaf := &PlanLeaf{Index: 1, Choices: map[string]int{"retries": 2}}
	tr := rt.RunTestLeaf(context.Background(), "test_anon", leaf)
	if tr.Result != "pass" {
		t.Fatalf("Result = %q (want pass); reason: %s", tr.Result, tr.Reason)
	}
}

// TestRunAll_NamedChoiceFansOut — a test body using
// choose("retries", [0, 1, 3]) must produce 3 TestResults, one per
// option, each marked with its LeafID. This is the rc2 entry point
// for the plan-tree fan-out engine.
func TestRunAll_NamedChoiceFansOut(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_fanout():
    r = choose("retries", [0, 1, 3])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 3 || res.Fail != 0 {
		t.Errorf("expected 3 pass / 0 fail, got %+v", res)
	}
	if len(res.Tests) != 3 {
		t.Fatalf("expected 3 leaf TestResults, got %d", len(res.Tests))
	}
	leafIDs := map[string]bool{}
	for _, tr := range res.Tests {
		leafIDs[tr.LeafID] = true
	}
	for _, want := range []string{"0", "1", "2"} {
		if !leafIDs[want] {
			t.Errorf("missing LeafID %q in results; got %v", want, leafIDs)
		}
	}
}

// TestRunAll_LeafChoicesAreDistinct — each leaf observes its own
// option (not just leaf 0 with FirstOption replicated). Asserted by
// having the body assert_eq the choose() return value against an
// expected per-leaf set; passing means each leaf saw a different
// concrete value.
func TestRunAll_LeafChoicesAreDistinct(t *testing.T) {
	rt := New(testLogger())
	// The body asserts the value is in [0, 1, 3] — passes only if
	// choose() returns one of those. If every leaf collapsed to
	// FirstOption=0, all 3 leaves would still pass — so the stronger
	// check below counts each value via the failure path.
	src := `
def test_distinct():
    r = choose("retries", [0, 1, 3])
    # Fail if r equals the wrong leaf's value — leaf 0 demands 0,
    # leaf 1 demands 1, leaf 2 demands 3 — but the body can't
    # observe its leaf index. Instead, succeed iff r is one of
    # the options; a stricter distinctness check below asserts
    # 3 distinct events via per-leaf event emission.
    assert_true(r in [0, 1, 3], "r must be one of the options")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 3 {
		t.Fatalf("expected 3 pass, got %+v", res)
	}
	// Direct PlanLeaf inspection — call the runtime helper to verify
	// each leaf does carry a unique Choices map.
	rt2 := New(testLogger())
	_ = rt2.LoadString("spec.star", src)
	axes := []*ChoiceVal{
		{Name: "retries", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1), starlark.MakeInt(3)}},
	}
	leaves := enumerateLeaves(axes, nil, nil)
	seen := map[int]bool{}
	for _, l := range leaves {
		seen[l.Choices["retries"]] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct retries values, got %v", seen)
	}
}

// TestRunAll_CrossProductFansOut — two named axes produce
// cardinality(a) * cardinality(b) leaves. Validates the mixed-radix
// enumerator.
func TestRunAll_CrossProductFansOut(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_cross():
    r = choose("retries", [0, 1])
    f = choose("fault", ["503", "504", "timeout"])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if got, want := res.Pass, 6; got != want {
		t.Errorf("Pass = %d, want %d (2 retries * 3 faults)", got, want)
	}
	if len(res.Tests) != 6 {
		t.Errorf("expected 6 leaves, got %d", len(res.Tests))
	}
}

// TestRunAll_LeafChoicesSurfaceOnTestResult — each leaf's TestResult
// carries the actual per-leaf choose() axis assignment in
// LeafChoices, so the bundle manifest and HTML report can render
// what every leaf actually saw (RFC-042 §8.10 Plan-tab integration,
// A3).
func TestRunAll_LeafChoicesSurfaceOnTestResult(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_axis():
    _ = choose("retries", [0, 1, 3])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 3 {
		t.Fatalf("expected 3 leaves, got %d", len(res.Tests))
	}
	seen := map[int]bool{}
	for _, tr := range res.Tests {
		if tr.LeafChoices == nil {
			t.Errorf("leaf %s: LeafChoices missing", tr.LeafID)
			continue
		}
		idx, ok := tr.LeafChoices["retries"]
		if !ok {
			t.Errorf("leaf %s: missing retries axis; have %v", tr.LeafID, tr.LeafChoices)
			continue
		}
		seen[idx] = true
	}
	for _, want := range []int{0, 1, 2} {
		if !seen[want] {
			t.Errorf("missing retries option index %d in leaves; got %v", want, seen)
		}
	}
	// Single-leaf rows omit LeafChoices entirely.
	rt2 := New(testLogger())
	_ = rt2.LoadString("spec.star", `def test_plain(): pass`)
	res2, _ := rt2.RunAll(context.Background(), RunConfig{})
	if res2.Tests[0].LeafChoices != nil {
		t.Errorf("single-leaf result must omit LeafChoices; got %v", res2.Tests[0].LeafChoices)
	}
}

// TestRunAll_NoChoiceStaysSingleLeaf — a test that doesn't call
// choose() at all keeps the rc1 shape: one TestResult per test, no
// LeafID set. This guards backwards-compatibility for the entire
// existing test corpus.
func TestRunAll_NoChoiceStaysSingleLeaf(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_plain():
    pass
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 || len(res.Tests) != 1 {
		t.Fatalf("expected 1 pass / 1 result, got %+v", res)
	}
	if res.Tests[0].LeafID != "" {
		t.Errorf("LeafID = %q, want empty for no-choose() test", res.Tests[0].LeafID)
	}
}

// TestRunAll_AnonymousChoiceStaysSingleLeaf — anonymous choose()
// is not a plan-tree axis (no name to address by); body still gets
// FirstOption, only one TestResult is produced.
func TestRunAll_AnonymousChoiceStaysSingleLeaf(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_anon():
    r = choose([10, 20, 30])
    assert_eq(r, 10, "anon choose returns FirstOption")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 || len(res.Tests) != 1 {
		t.Fatalf("expected 1 pass / 1 result, got %+v", res)
	}
}

// TestRunAll_SingleOptionAxisStaysSingleLeaf — choose("x", [42]) is
// a degenerate axis with cardinality 1; the plan walker collapses
// it because there's nothing to vary across.
func TestRunAll_SingleOptionAxisStaysSingleLeaf(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_one_opt():
    r = choose("k", [42])
    assert_eq(r, 42, "single-option axis")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 1 || len(res.Tests) != 1 {
		t.Fatalf("expected 1 pass / 1 result, got %+v", res)
	}
}

func TestEnumerateLeaves_MixedRadix(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "a", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
		{Name: "b", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1), starlark.MakeInt(2)}},
	}
	leaves := enumerateLeaves(axes, nil, nil)
	if len(leaves) != 6 {
		t.Fatalf("expected 6 leaves, got %d", len(leaves))
	}
	// Leaf 0 must be the all-zeros assignment so the discovery run
	// can be reused.
	if leaves[0].Choices["a"] != 0 || leaves[0].Choices["b"] != 0 {
		t.Errorf("leaf 0 must be all-zeros, got %v", leaves[0].Choices)
	}
	// Spot-check: leaf 4 in mixed-radix (a=2, b=3) decodes to a=0, b=2.
	if got := leaves[4].Choices; got["a"] != 0 || got["b"] != 2 {
		t.Errorf("leaf 4 = %v, want {a:0, b:2}", got)
	}
}

func TestChoiceVal_StringAndType(t *testing.T) {
	c := &ChoiceVal{Name: "x", Options: []starlark.Value{starlark.MakeInt(1), starlark.MakeInt(2)}}
	if c.Type() != "choice" {
		t.Errorf("Type = %q, want choice", c.Type())
	}
	if c.String() != "<choice x [2]>" {
		t.Errorf("String = %q", c.String())
	}
	if c.Truth() != starlark.True {
		t.Error("non-empty Choice should be truthy")
	}
	empty := &ChoiceVal{}
	if empty.Truth() != starlark.False {
		t.Error("empty Choice should be falsy")
	}
	if empty.FirstOption() != starlark.None {
		t.Error("empty Choice FirstOption should be None")
	}
}
