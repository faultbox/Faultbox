package star

// PlanLeaf is one branch of the plan tree — a concrete assignment of
// every non-deterministic axis the spec exposes. The plan-tree
// enumerator (RFC-042 §8.8/§8.9, landing across this rc2 slice)
// produces one PlanLeaf per leaf and feeds each into RunTestLeaf for
// independent execution.
//
// Today the fields cover only named choose() axes (RFC-043 §5.2).
// Probability fan-out (§8.9) adds ProbabilityOutcomes per fault firing
// site in the next slice; interleaving execution (§8.8) adds an
// InterleavingID that the engine consumes via the existing
// hold-and-release substrate.
//
// The zero PlanLeaf (nil pointer) means "single-leaf degenerate
// execution" — every builtin falls back to its rc1 default
// (choose()→first option, faults→stochastic). RunTest threads nil
// through the entire pre-rc2 call surface so existing tests keep
// passing unmodified.
type PlanLeaf struct {
	// Index is the leaf's ordinal in the plan tree, used to derive a
	// stable LeafID like "test_foo[1]" when the test name alone would
	// collide across leaves.
	Index int

	// Choices maps a named choose("name", [...]) call to its selected
	// option index. Lookup is by name; anonymous choose() calls (no
	// name) are not part of the plan tree in rc2 — they keep
	// returning the first option (consistent with rc1).
	Choices map[string]int
}

// optionIndex returns (idx, true) when the leaf pins this named
// axis to a specific option; (0, false) otherwise.
func (l *PlanLeaf) optionIndex(name string) (int, bool) {
	if l == nil || name == "" {
		return 0, false
	}
	i, ok := l.Choices[name]
	return i, ok
}
