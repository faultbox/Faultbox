package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestInterleavings_Parsing covers the int + string + omitted cases
// for the RFC-042 §8.8 interleavings= kwarg. Validation is centralized
// in parseInterleavingsKwarg so it stays consistent if/when wait_all,
// wait_n, wait_first land alongside parallel() in a later slice.
func TestInterleavings_Parsing(t *testing.T) {
	mkKwargs := func(v starlark.Value) []starlark.Tuple {
		return []starlark.Tuple{{starlark.String("interleavings"), v}}
	}
	cases := []struct {
		name    string
		kwargs  []starlark.Tuple
		wantKnd string
		wantN   int
	}{
		{"omitted defaults to single", nil, "single", 0},
		{"int 1 collapses to single", mkKwargs(starlark.MakeInt(1)), "single", 0},
		{"int N stored as n-cap", mkKwargs(starlark.MakeInt(4)), "n", 4},
		{"string \"1\" collapses to single", mkKwargs(starlark.String("1")), "single", 0},
		{"string \"all\"", mkKwargs(starlark.String("all")), "all", 0},
		{"string \"critical\"", mkKwargs(starlark.String("critical")), "critical", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseInterleavingsKwarg("parallel", tc.kwargs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Kind != tc.wantKnd || p.N != tc.wantN {
				t.Errorf("got %+v, want Kind=%q N=%d", p, tc.wantKnd, tc.wantN)
			}
		})
	}
}

// TestInterleavings_Rejections — reserved values and bad inputs must
// surface explicit messages so CI integrations gating on the kwarg
// don't drift when RFC-009/DPOR lands.
func TestInterleavings_Rejections(t *testing.T) {
	mkKwargs := func(v starlark.Value) []starlark.Tuple {
		return []starlark.Tuple{{starlark.String("interleavings"), v}}
	}
	cases := []struct {
		name     string
		kwargs   []starlark.Tuple
		wantSubs string
	}{
		{"int 0 rejected", mkKwargs(starlark.MakeInt(0)), "positive integer"},
		{"int negative rejected", mkKwargs(starlark.MakeInt(-3)), "positive integer"},
		{"string unknown rejected", mkKwargs(starlark.String("random")), "must be 1, an integer N"},
		{"dpor reserved with RFC ref", mkKwargs(starlark.String("dpor")), "RFC-009"},
		{"sut-internal reserved with L5 ref", mkKwargs(starlark.String("sut-internal")), "L5"},
		{"non-int non-string rejected", mkKwargs(starlark.True), "must be int or string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseInterleavingsKwarg("parallel", tc.kwargs)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestInterleavings_PolicyString — stable string form for plan.json
// and bundle manifest consumers.
func TestInterleavings_PolicyString(t *testing.T) {
	cases := []struct {
		p    InterleavingPolicy
		want string
	}{
		{InterleavingPolicy{Kind: "single"}, "1"},
		{InterleavingPolicy{Kind: "all"}, "all"},
		{InterleavingPolicy{Kind: "critical"}, "critical"},
		{InterleavingPolicy{Kind: "n", N: 5}, "5"},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("policy %+v: String()=%q, want %q", tc.p, got, tc.want)
		}
	}
}

// TestInterleavings_ParallelAcceptsKwarg — wiring sanity. The kwarg
// is accepted at the parallel() call surface today; the plan walker
// that consumes it lands in PR 3 of this slice. For now we just
// verify spec load succeeds with the new kwarg present.
func TestInterleavings_ParallelAcceptsKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def body():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
}

// TestInterleavings_RecordsParallelSite — calling parallel(...,
// interleavings="all") during a test body records a ParallelSite
// the plan walker will read in PR 3 of this slice. Key is the
// file:line of the call so dedup across re-entry works.
func TestInterleavings_RecordsParallelSite(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_record():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_record")
	if tr.Result != "pass" {
		t.Fatalf("Result = %q, want pass; reason: %s", tr.Result, tr.Reason)
	}
	sites := rt.bodyParallelSites()
	if len(sites) != 1 {
		t.Fatalf("expected 1 recorded parallel site, got %d: %+v", len(sites), sites)
	}
	if sites[0].Branches != 2 {
		t.Errorf("Branches = %d, want 2", sites[0].Branches)
	}
	if sites[0].Policy.Kind != "all" {
		t.Errorf("Policy.Kind = %q, want all", sites[0].Policy.Kind)
	}
	if !strings.Contains(sites[0].Key, "spec.star") {
		t.Errorf("Key should embed call-site path; got %q", sites[0].Key)
	}
}

// TestInterleavings_SingleKindIgnoredByEnumerator — recording still
// happens for policy.Kind="single" (the default), so plan output can
// surface "parallel(N branches, interleavings=1)" lines for debug
// even though no fan-out occurs. The enumerator in PR 3 filters
// these out when computing axes.
func TestInterleavings_RecordsEvenSinglePolicy(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_single():
    parallel(a, b)  # default interleavings=1
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_single")
	if tr.Result != "pass" {
		t.Fatalf("Result = %q, want pass; reason: %s", tr.Result, tr.Reason)
	}
	sites := rt.bodyParallelSites()
	if len(sites) != 1 {
		t.Fatalf("expected 1 recorded parallel site (single policy still recorded), got %d", len(sites))
	}
	if sites[0].Policy.Kind != "single" {
		t.Errorf("Policy.Kind = %q, want single", sites[0].Policy.Kind)
	}
}

// TestInterleavings_DedupByKey — re-entering the same parallel()
// statement (e.g. via a loop) records the call only once so the
// axis cardinality doesn't double.
func TestInterleavings_DedupByKey(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_dedup():
    for _ in range(3):
        parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_dedup")
	if tr.Result != "pass" {
		t.Fatalf("Result = %q, want pass; reason: %s", tr.Result, tr.Reason)
	}
	sites := rt.bodyParallelSites()
	if len(sites) != 1 {
		t.Errorf("expected 1 site (dedup by key), got %d: %+v", len(sites), sites)
	}
}

// TestInterleavings_ResetBetweenLeaves — runTestFanout's reset path
// clears parallel-site recordings. A second test body run starts
// with no recorded sites.
func TestInterleavings_ResetBetweenLeaves(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_reset():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	_ = rt.RunTest(context.Background(), "test_reset")
	if got := len(rt.bodyParallelSites()); got != 1 {
		t.Fatalf("expected 1 site after first run, got %d", got)
	}
	rt.resetBodyParallelSites()
	if got := len(rt.bodyParallelSites()); got != 0 {
		t.Errorf("expected 0 sites after reset, got %d", got)
	}
}

// TestInterleavings_Cardinality — per-policy leaf counts the plan
// walker uses to size the cross-product.
func TestInterleavings_Cardinality(t *testing.T) {
	cases := []struct {
		site ParallelSite
		want int
	}{
		{ParallelSite{Branches: 2, Policy: InterleavingPolicy{Kind: "single"}}, 0},
		{ParallelSite{Branches: 1, Policy: InterleavingPolicy{Kind: "all"}}, 0},
		{ParallelSite{Branches: 2, Policy: InterleavingPolicy{Kind: "all"}}, 2}, // 2!
		{ParallelSite{Branches: 3, Policy: InterleavingPolicy{Kind: "all"}}, 6}, // 3!
		{ParallelSite{Branches: 4, Policy: InterleavingPolicy{Kind: "all"}}, 24},
		{ParallelSite{Branches: 5, Policy: InterleavingPolicy{Kind: "n", N: 3}}, 3},   // cap
		{ParallelSite{Branches: 3, Policy: InterleavingPolicy{Kind: "n", N: 100}}, 6}, // factorial smaller than cap
		{ParallelSite{Branches: 3, Policy: InterleavingPolicy{Kind: "critical"}}, 5},  // 2*3-1
		{ParallelSite{Branches: 2, Policy: InterleavingPolicy{Kind: "critical"}}, 3},  // 2*2-1
	}
	for _, tc := range cases {
		if got := interleavingCardinality(tc.site); got != tc.want {
			t.Errorf("cardinality(%+v) = %d, want %d", tc.site, got, tc.want)
		}
	}
}

// TestEnumerateLeaves_ParallelOnly — one parallel site with
// interleavings="all", 3 branches → 6 leaves with distinct
// InterleavingIDs.
func TestEnumerateLeaves_ParallelOnly(t *testing.T) {
	par := []ParallelSite{{
		Key: "spec.star:5", Branches: 3,
		Policy: InterleavingPolicy{Kind: "all"},
	}}
	leaves := enumerateLeaves(nil, nil, par)
	if len(leaves) != 6 {
		t.Fatalf("expected 6 leaves (3!), got %d", len(leaves))
	}
	seen := make(map[int]bool)
	for _, l := range leaves {
		idx, ok := l.InterleavingIndex("spec.star:5")
		if !ok {
			t.Errorf("leaf %d missing interleaving id", l.Index)
			continue
		}
		seen[idx] = true
	}
	if len(seen) != 6 {
		t.Errorf("expected 6 distinct interleaving indices, got %d: %v", len(seen), seen)
	}
}

// TestEnumerateLeaves_ChoiceProbAndParCrossProduct — full
// cross-product: 2 choices × 4 prob leaves × 3 interleavings = 24
// leaves.
func TestEnumerateLeaves_ChoiceProbAndParCrossProduct(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "k", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
	}
	prob := []ProbFaultSite{{Key: "wal", MaxFires: 2, Prob: 0.3}}
	par := []ParallelSite{{
		Key: "spec.star:7", Branches: 3,
		Policy: InterleavingPolicy{Kind: "n", N: 3},
	}}
	leaves := enumerateLeaves(axes, prob, par)
	if len(leaves) != 24 {
		t.Errorf("2 × 4 × 3 = 24, got %d", len(leaves))
	}
}

// TestEnumerateLeaves_SinglePolicyParAxisCollapses — a single-policy
// parallel site doesn't multiply leaf count, even when other axes
// fan out.
func TestEnumerateLeaves_SinglePolicyParAxisCollapses(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "k", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
	}
	par := []ParallelSite{{
		Key: "spec.star:9", Branches: 5,
		Policy: InterleavingPolicy{Kind: "single"},
	}}
	leaves := enumerateLeaves(axes, nil, par)
	if len(leaves) != 2 {
		t.Fatalf("single-policy par axis must not multiply leaves, got %d", len(leaves))
	}
	if _, ok := leaves[0].InterleavingIndex("spec.star:9"); ok {
		t.Error("single-policy site must not produce an interleaving pin in the leaf")
	}
}

// TestPlanLeaf_InterleavingIndex — accessor contract.
func TestPlanLeaf_InterleavingIndex(t *testing.T) {
	leaf := &PlanLeaf{InterleavingIDs: map[string]int{"k1": 3}}
	if got, ok := leaf.InterleavingIndex("k1"); !ok || got != 3 {
		t.Errorf("got %d/%v, want 3/true", got, ok)
	}
	if _, ok := leaf.InterleavingIndex("missing"); ok {
		t.Error("missing key must report unpinned")
	}
	var nilLeaf *PlanLeaf
	if _, ok := nilLeaf.InterleavingIndex("k1"); ok {
		t.Error("nil leaf must report unpinned")
	}
}

// TestRunAll_ParallelInterleavingsFansOut — running a spec whose
// body calls parallel(a, b, interleavings="all") with 2 branches
// produces 2 leaves (2!). Each leaf carries a distinct LeafID and
// the engine-execution sees a different InterleavingID assignment
// in PR 4 of this slice; for now the recording + enumeration is
// enough to drive RunAll's leaf loop.
func TestRunAll_ParallelInterleavingsFansOut(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_par_fanout():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if res.Pass != 2 {
		t.Errorf("expected 2 pass (2!), got %+v", res)
	}
	if len(res.Tests) != 2 {
		t.Fatalf("expected 2 leaf rows, got %d", len(res.Tests))
	}
	leafIDs := map[string]bool{}
	for _, tr := range res.Tests {
		leafIDs[tr.LeafID] = true
	}
	if !leafIDs["0"] || !leafIDs["1"] {
		t.Errorf("expected LeafIDs {0, 1}, got %v", leafIDs)
	}
}

// TestRunAll_ParallelSingleStaysOneLeaf — without interleavings= or
// with the default policy, parallel() runs once, producing one
// TestResult with empty LeafID (rc2 PR-1 behavior preserved).
func TestRunAll_ParallelSingleStaysOneLeaf(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_single():
    parallel(a, b)
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
		t.Errorf("LeafID = %q, want empty (no fan-out)", res.Tests[0].LeafID)
	}
}

// TestInterleavings_ParallelRejectsBadKwarg — bad value surfaces at
// runtime (parallel() is body-time, not load-time).
func TestInterleavings_ParallelRejectsBadKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_bad():
    parallel(a, b, interleavings="dpor")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_bad")
	if tr.Result != "fail" {
		t.Fatalf("Result = %q, want fail; reason: %s", tr.Result, tr.Reason)
	}
	if !strings.Contains(tr.Reason, "RFC-009") {
		t.Errorf("Reason should cite RFC-009; got %q", tr.Reason)
	}
}
