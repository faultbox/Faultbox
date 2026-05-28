package star

import (
	"reflect"
	"sort"
	"testing"

	"go.starlark.net/starlark"
)

// TestNonDet_ChoiceValInterface — ChoiceVal implements
// NonDeterministicChoice with the rc2 cardinality / apply
// semantics: anonymous axes and single-option axes contribute 0
// leaves; named multi-option axes contribute len(Options).
func TestNonDet_ChoiceValInterface(t *testing.T) {
	var _ NonDeterministicChoice = (*ChoiceVal)(nil) // compile-time witness

	anon := &ChoiceVal{Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}}
	if anon.Cardinality() != 0 {
		t.Errorf("anonymous Choice: Cardinality = %d, want 0", anon.Cardinality())
	}

	single := &ChoiceVal{Name: "k", Options: []starlark.Value{starlark.MakeInt(42)}}
	if single.Cardinality() != 0 {
		t.Errorf("single-option Choice: Cardinality = %d, want 0", single.Cardinality())
	}

	named := &ChoiceVal{Name: "retries", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1), starlark.MakeInt(3)}}
	if got, want := named.Cardinality(), 3; got != want {
		t.Errorf("named Choice: Cardinality = %d, want %d", got, want)
	}

	leaf := &PlanLeaf{}
	named.Apply(leaf, 2)
	if leaf.Choices["retries"] != 2 {
		t.Errorf("Apply did not pin axis: got %v", leaf.Choices)
	}
}

// TestNonDet_ProbFaultSiteInterface — ProbFaultSite implements
// NonDeterministicChoice. Cardinality = 2^MaxFires, Apply stamps
// a bit-vector decoded from digit.
func TestNonDet_ProbFaultSiteInterface(t *testing.T) {
	var _ NonDeterministicChoice = ProbFaultSite{} // compile-time witness

	zero := ProbFaultSite{Key: "wal", MaxFires: 0}
	if zero.Cardinality() != 0 {
		t.Errorf("MaxFires=0: Cardinality = %d, want 0", zero.Cardinality())
	}

	site := ProbFaultSite{Key: "wal", MaxFires: 3}
	if got, want := site.Cardinality(), 8; got != want {
		t.Errorf("MaxFires=3: Cardinality = %d, want %d", got, want)
	}

	leaf := &PlanLeaf{}
	site.Apply(leaf, 5) // 101 → [true, false, true]
	want := []bool{true, false, true}
	if !reflect.DeepEqual(leaf.ProbabilityOutcomes["wal"], want) {
		t.Errorf("digit=5: got %v, want %v", leaf.ProbabilityOutcomes["wal"], want)
	}
}

// TestNonDet_ParallelSiteInterface — ParallelSite implements
// NonDeterministicChoice. Cardinality delegates to
// interleavingCardinality (already policy-aware); Apply stamps
// the ordering index.
func TestNonDet_ParallelSiteInterface(t *testing.T) {
	var _ NonDeterministicChoice = ParallelSite{} // compile-time witness

	single := ParallelSite{Key: "spec:1", Branches: 3, Policy: InterleavingPolicy{Kind: "single"}}
	if single.Cardinality() != 0 {
		t.Errorf("single policy: Cardinality = %d, want 0 (no fan-out)", single.Cardinality())
	}

	all := ParallelSite{Key: "spec:1", Branches: 3, Policy: InterleavingPolicy{Kind: "all"}}
	if got, want := all.Cardinality(), 6; got != want {
		t.Errorf("3! all: Cardinality = %d, want %d", got, want)
	}

	leaf := &PlanLeaf{}
	all.Apply(leaf, 4)
	if leaf.InterleavingIDs["spec:1"] != 4 {
		t.Errorf("Apply did not pin index: got %v", leaf.InterleavingIDs)
	}
}

// TestNonDet_CollectChoicesPreservesOrder — collectChoices flattens
// the three source slices in (axes, probAxes, parAxes) order and
// filters out zero-cardinality axes. Stable order is what keeps
// plan-tree output byte-identical across runs.
func TestNonDet_CollectChoicesPreservesOrder(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "a", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
		{Name: "", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}}, // anonymous — filtered
		{Name: "b", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
	}
	probAxes := []ProbFaultSite{
		{Key: "wal", MaxFires: 1},
		{Key: "noop", MaxFires: 0}, // filtered
	}
	parAxes := []ParallelSite{
		{Key: "spec:1", Branches: 2, Policy: InterleavingPolicy{Kind: "all"}},
		{Key: "spec:2", Branches: 2, Policy: InterleavingPolicy{Kind: "single"}}, // filtered
	}
	got := collectChoices(axes, probAxes, parAxes)
	if len(got) != 4 {
		t.Fatalf("expected 4 active axes, got %d", len(got))
	}
	// Type-check the ordering: choose axes first, then prob, then par.
	if _, ok := got[0].(*ChoiceVal); !ok {
		t.Errorf("got[0] = %T, want *ChoiceVal", got[0])
	}
	if _, ok := got[1].(*ChoiceVal); !ok {
		t.Errorf("got[1] = %T, want *ChoiceVal", got[1])
	}
	if _, ok := got[2].(ProbFaultSite); !ok {
		t.Errorf("got[2] = %T, want ProbFaultSite", got[2])
	}
	if _, ok := got[3].(ParallelSite); !ok {
		t.Errorf("got[3] = %T, want ParallelSite", got[3])
	}
}

// TestNonDet_ExpandLeavesIdenticalToEnumerateLeaves — RFC-044 §8.2
// safety net: expandLeaves(collectChoices(...)) must produce
// byte-identical PlanLeaf slices to the pre-refactor
// enumerateLeaves call. This is the user-facing plan-tree
// invariant the RFC explicitly promises.
func TestNonDet_ExpandLeavesIdenticalToEnumerateLeaves(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "a", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1)}},
		{Name: "b", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1), starlark.MakeInt(2)}},
	}
	probAxes := []ProbFaultSite{{Key: "wal", MaxFires: 2}}
	parAxes := []ParallelSite{{Key: "spec:5", Branches: 3, Policy: InterleavingPolicy{Kind: "all"}}}

	got := enumerateLeaves(axes, probAxes, parAxes)
	wantLen := 2 * 3 * 4 * 6 // 144
	if len(got) != wantLen {
		t.Fatalf("expected %d leaves (2 * 3 * 2^2 * 3!), got %d", wantLen, len(got))
	}
	// Spot check: leaf 0 carries all-zero assignments; leaf wantLen-1
	// carries the max-index assignment for every axis.
	if got[0].Choices["a"] != 0 || got[0].Choices["b"] != 0 {
		t.Errorf("leaf 0 choices = %v, want all-zero", got[0].Choices)
	}
	if got[wantLen-1].Choices["a"] != 1 || got[wantLen-1].Choices["b"] != 2 {
		t.Errorf("leaf %d choices = %v, want a=1,b=2", wantLen-1, got[wantLen-1].Choices)
	}
	if got[wantLen-1].InterleavingIDs["spec:5"] != 5 {
		t.Errorf("leaf %d interleaving = %v, want 5", wantLen-1, got[wantLen-1].InterleavingIDs)
	}
	// Leaf indices must be 0..wantLen-1 with no gaps.
	indices := make([]int, len(got))
	for i, l := range got {
		indices[i] = l.Index
	}
	sort.Ints(indices)
	for i := range indices {
		if indices[i] != i {
			t.Errorf("indices missing %d; got %v", i, indices)
			break
		}
	}
}

// TestNonDet_NoAxesProducesDegenerate — the empty input must still
// produce one leaf so callers iterate uniformly. This is what
// makes runTestFanout's "no fan-out" short-circuit valid.
func TestNonDet_NoAxesProducesDegenerate(t *testing.T) {
	got := expandLeaves(nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 degenerate leaf, got %d", len(got))
	}
	if got[0].Index != 0 {
		t.Errorf("Index = %d, want 0", got[0].Index)
	}
}
