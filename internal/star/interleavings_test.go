package star

import (
	"context"
	"fmt"
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
		// Review B1: 2-branch "critical" used to return 3, but the
		// 3rd leaf decoded to the same permutation as the 2nd
		// (permutationByIndex clamps). Cap at min(2N-1, N!) = 2.
		{ParallelSite{Branches: 2, Policy: InterleavingPolicy{Kind: "critical"}}, 2},
		// 4 branches "critical" stays at 2N-1=7 (well below 4!=24).
		{ParallelSite{Branches: 4, Policy: InterleavingPolicy{Kind: "critical"}}, 7},
	}
	for _, tc := range cases {
		if got := interleavingCardinality(tc.site); got != tc.want {
			t.Errorf("cardinality(%+v) = %d, want %d", tc.site, got, tc.want)
		}
	}
}

// TestInterleavings_CriticalLeavesAreDistinct — review B1
// regression: every leaf produced by "critical" must decode to a
// unique permutation. A clamp in permutationByIndex used to make
// the last leaf duplicate the second-to-last for 2-branch sites.
func TestInterleavings_CriticalLeavesAreDistinct(t *testing.T) {
	for n := 2; n <= 4; n++ {
		site := ParallelSite{Branches: n, Policy: InterleavingPolicy{Kind: "critical"}}
		card := interleavingCardinality(site)
		seen := map[string]bool{}
		for i := 0; i < card; i++ {
			ord := interleavingOrdering(site, i)
			key := fmt.Sprint(ord)
			if seen[key] {
				t.Errorf("n=%d: duplicate ordering at idx %d: %v", n, i, ord)
			}
			seen[key] = true
		}
	}
}

// TestInterleavings_ParallelRejectsUnknownKwarg — review B2
// regression: a typo like `interleaving=` (missing s) used to
// degrade silently to single-leaf. Now rejected with an explicit
// error.
func TestInterleavings_ParallelRejectsUnknownKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_typo():
    parallel(a, b, interleaving="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_typo")
	if tr.Result != "fail" {
		t.Fatalf("Result = %q, want fail; reason: %s", tr.Result, tr.Reason)
	}
	if !strings.Contains(tr.Reason, "unrecognized keyword argument") || !strings.Contains(tr.Reason, "interleaving") {
		t.Errorf("Reason should call out the typo; got %q", tr.Reason)
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

// TestInterleavings_PermutationByIndex — Lehmer-code decode pins
// stable orderings the engine and plan walker agree on.
func TestInterleavings_PermutationByIndex(t *testing.T) {
	cases := []struct {
		n, k int
		want []int
	}{
		{3, 0, []int{0, 1, 2}},
		{3, 1, []int{0, 2, 1}},
		{3, 2, []int{1, 0, 2}},
		{3, 3, []int{1, 2, 0}},
		{3, 4, []int{2, 0, 1}},
		{3, 5, []int{2, 1, 0}},
		{2, 0, []int{0, 1}},
		{2, 1, []int{1, 0}},
	}
	for _, tc := range cases {
		got := permutationByIndex(tc.n, tc.k)
		if len(got) != len(tc.want) {
			t.Errorf("permutationByIndex(%d, %d) len = %d, want %d", tc.n, tc.k, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("permutationByIndex(%d, %d) = %v, want %v", tc.n, tc.k, got, tc.want)
				break
			}
		}
	}
}

// TestInterleavings_OrderingByPolicy — interleavingOrdering returns
// the right Lehmer permutation for each policy + index.
func TestInterleavings_OrderingByPolicy(t *testing.T) {
	site := func(kind string, n int) ParallelSite {
		return ParallelSite{Branches: 3, Policy: InterleavingPolicy{Kind: kind, N: n}}
	}
	if got := interleavingOrdering(site("all", 0), 0); got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("all[0] = %v, want identity", got)
	}
	if got := interleavingOrdering(site("all", 0), 5); got[0] != 2 || got[1] != 1 || got[2] != 0 {
		t.Errorf("all[5] = %v, want reverse", got)
	}
	if got := interleavingOrdering(site("n", 2), 5); got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("n=2, out-of-cap idx must fall back to identity; got %v", got)
	}
}

// TestRunAll_LeafDrivenBranchOrder — leaves 0 and 1 of a 2-branch
// parallel fan-out execute under the parallelWithLeaf path. Smoke
// check; the per-branch invocation order is asserted directly by
// TestParallelWithLeaf_LaunchesBranchesInOrder below (review N2 on
// PR #123 — that test addresses the "only guards against panic"
// concern by observing the ordering through Go-side branches).
func TestRunAll_LeafDrivenBranchOrder(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_order():
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
		t.Fatalf("expected 2 pass, got %+v", res)
	}
}

// TestParallelWithLeaf_LaunchesBranchesInOrder — direct test of the
// engine path: pass a known ordering and observe the Go-side
// branch-invocation sequence. Two calls with different orderings
// must produce different sequences. Addresses review N2 on PR #123
// — without this a regression where parallelWithLeaf always used
// identity ordering would still pass the suite.
func TestParallelWithLeaf_LaunchesBranchesInOrder(t *testing.T) {
	rt := New(testLogger())
	mkBranch := func(seen *[]int, i int) starlark.Callable {
		idx := i
		return starlark.NewBuiltin(
			"b",
			func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
				*seen = append(*seen, idx)
				return starlark.None, nil
			},
		)
	}

	var seenA []int
	branchesA := []starlark.Callable{mkBranch(&seenA, 0), mkBranch(&seenA, 1), mkBranch(&seenA, 2)}
	if _, err := rt.parallelWithLeaf(branchesA, []int{2, 0, 1}); err != nil {
		t.Fatalf("parallelWithLeaf A: %v", err)
	}
	wantA := []int{2, 0, 1}
	if len(seenA) != len(wantA) {
		t.Fatalf("seenA = %v, want %v", seenA, wantA)
	}
	for i := range wantA {
		if seenA[i] != wantA[i] {
			t.Fatalf("seenA = %v, want %v", seenA, wantA)
		}
	}

	var seenB []int
	branchesB := []starlark.Callable{mkBranch(&seenB, 0), mkBranch(&seenB, 1), mkBranch(&seenB, 2)}
	if _, err := rt.parallelWithLeaf(branchesB, []int{1, 2, 0}); err != nil {
		t.Fatalf("parallelWithLeaf B: %v", err)
	}
	wantB := []int{1, 2, 0}
	if len(seenB) != len(wantB) {
		t.Fatalf("seenB = %v, want %v", seenB, wantB)
	}
	for i := range wantB {
		if seenB[i] != wantB[i] {
			t.Fatalf("seenB = %v, want %v", seenB, wantB)
		}
	}
}

// TestParallelWithLeaf_SkipsOutOfRangeIndex — a buggy enumerator
// passing an out-of-range index must not crash; the bounds check
// in parallelWithLeaf skips it and the remaining branches run.
func TestParallelWithLeaf_SkipsOutOfRangeIndex(t *testing.T) {
	rt := New(testLogger())
	var seen []int
	mk := func(i int) starlark.Callable {
		idx := i
		return starlark.NewBuiltin(
			"b",
			func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
				seen = append(seen, idx)
				return starlark.None, nil
			},
		)
	}
	branches := []starlark.Callable{mk(0), mk(1)}
	if _, err := rt.parallelWithLeaf(branches, []int{-1, 99, 1, 0}); err != nil {
		t.Fatalf("parallelWithLeaf: %v", err)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 0 {
		t.Errorf("seen = %v, want [1 0] (out-of-range skipped)", seen)
	}
}

// TestRunAll_LeafInterleavingsSurfaceOnTestResult — interleaving
// axes flow into TestResult.LeafInterleavingIDs (A3 / RFC-042 §8.10).
// The HTML report's formatLeafAxes() consumes this through the
// bundle's leaf_interleavings field to render per-leaf labels.
//
// Review N3 on PR #124: assert the two leaves carry DISTINCT
// interleaving indices, not just that each has an entry. A
// regression to always-index-0 would pass the bare count check.
func TestRunAll_LeafInterleavingsSurfaceOnTestResult(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_par_axis():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 2 {
		t.Fatalf("expected 2 leaves, got %d", len(res.Tests))
	}
	seenIdx := map[int]bool{}
	var seenKey string
	for _, tr := range res.Tests {
		if len(tr.LeafInterleavingIDs) != 1 {
			t.Errorf("leaf %s: expected 1 interleaving entry, got %v", tr.LeafID, tr.LeafInterleavingIDs)
			continue
		}
		for k, idx := range tr.LeafInterleavingIDs {
			if seenKey == "" {
				seenKey = k
			} else if k != seenKey {
				t.Errorf("leaves disagree on parallel site key: %q vs %q", seenKey, k)
			}
			seenIdx[idx] = true
		}
	}
	for _, want := range []int{0, 1} {
		if !seenIdx[want] {
			t.Errorf("missing interleaving index %d across leaves; got %v", want, seenIdx)
		}
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
