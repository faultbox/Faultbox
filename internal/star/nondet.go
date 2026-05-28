package star

// NonDeterministicChoice is the unified plan-tree fan-out axis
// (RFC-044 §8.2). Every axis kind — named `choose()` axes,
// probability fault sites, parallel() interleavings — implements
// this interface so enumerateLeaves can walk a homogeneous slice
// instead of branching by type.
//
// Implementations must be pure: Cardinality() is read once before
// the cross-product is sized, and Apply() is called exactly once
// per (leaf, axis) pair with digit ∈ [0, Cardinality()-1]. Neither
// method may mutate the receiver across calls — the same axis is
// applied to every leaf with a different digit.
//
// Why an interface rather than a sum type: rc2's three axis kinds
// (ChoiceVal, ProbFaultSite, ParallelSite) live in three files and
// each carries kind-specific metadata (option lists, max-fires
// counts, ordering keys). Forcing them into one struct would
// duplicate fields; an interface keeps each kind in its own file
// and only requires the two-method contract for fan-out
// participation.
//
// Scope note: fault_matrix and fault_scenario are NOT
// NonDeterministicChoices — they produce discrete *test* entries
// at spec load time, not *leaves* of one test at body time. The
// plan walker in internal/plan/ handles those separately.
type NonDeterministicChoice interface {
	// Cardinality returns the number of leaves this axis contributes
	// to the cross-product. Must return ≥ 1 for the axis to
	// participate; returning 0 means "no fan-out" (the axis is
	// recorded for plan output but doesn't multiply leaves).
	Cardinality() int

	// Apply stamps the per-leaf assignment for this axis onto leaf,
	// using the supplied digit (0-based, < Cardinality()). The
	// implementation decides which PlanLeaf field gets updated.
	Apply(leaf *PlanLeaf, digit int)
}

// Cardinality / Apply implementations for the three axis kinds
// follow. Kept here rather than in choose.go / leaf.go /
// interleavings.go so the interface contract sits next to its
// implementations and a reviewer can verify all three implement it
// identically.

// Cardinality on ChoiceVal — number of options for a named axis.
// Anonymous axes (Name=="") and single-option axes return 0; the
// caller filters those out before building the enumeration list.
func (c *ChoiceVal) Cardinality() int {
	if c == nil || c.Name == "" || len(c.Options) < 2 {
		return 0
	}
	return len(c.Options)
}

// Apply on ChoiceVal — pin leaf.Choices[Name] to the digit.
// Lazily allocates the map so a leaf with no axes stays nil.
func (c *ChoiceVal) Apply(leaf *PlanLeaf, digit int) {
	if c == nil || c.Name == "" || leaf == nil {
		return
	}
	if leaf.Choices == nil {
		leaf.Choices = make(map[string]int)
	}
	leaf.Choices[c.Name] = digit
}

// Cardinality on ProbFaultSite — 2^MaxFires (one leaf per
// per-occurrence fire/no-fire combination). Returns 0 for sites
// without max_fires set; those don't fan out (stochastic mode).
func (p ProbFaultSite) Cardinality() int {
	if p.MaxFires <= 0 {
		return 0
	}
	return 1 << p.MaxFires
}

// Apply on ProbFaultSite — decode digit as a binary vector and
// stamp onto leaf.ProbabilityOutcomes[Key].
func (p ProbFaultSite) Apply(leaf *PlanLeaf, digit int) {
	if p.Key == "" || p.MaxFires <= 0 || leaf == nil {
		return
	}
	vec := make([]bool, p.MaxFires)
	for b := 0; b < p.MaxFires; b++ {
		vec[b] = (digit>>b)&1 == 1
	}
	if leaf.ProbabilityOutcomes == nil {
		leaf.ProbabilityOutcomes = make(map[string][]bool)
	}
	leaf.ProbabilityOutcomes[p.Key] = vec
}

// Cardinality on ParallelSite — delegates to the policy-aware
// helper interleavingCardinality (already capped at min(2N-1, N!)
// for "critical" and min(N, N!) for "n").
func (p ParallelSite) Cardinality() int {
	return interleavingCardinality(p)
}

// Apply on ParallelSite — pin leaf.InterleavingIDs[Key] to the
// digit. The digit is the index into the policy's ordering
// enumeration; interleavingOrdering decodes it to a concrete
// branch permutation at execution time.
func (p ParallelSite) Apply(leaf *PlanLeaf, digit int) {
	if p.Key == "" || leaf == nil {
		return
	}
	if leaf.InterleavingIDs == nil {
		leaf.InterleavingIDs = make(map[string]int)
	}
	leaf.InterleavingIDs[p.Key] = digit
}

// collectChoices flattens the three recorded discovery slices into
// a single []NonDeterministicChoice, filtering out axes whose
// cardinality is 0 (anonymous choose() calls, single-option axes,
// single-policy parallel sites, no-max-fires probability rules).
// The result preserves the source order: named choose() axes
// first, then probability sites, then parallel sites. Stable
// ordering matters for the plan tree to stay byte-identical across
// runs given the same recordings.
func collectChoices(
	axes []*ChoiceVal,
	probAxes []ProbFaultSite,
	parAxes []ParallelSite,
) []NonDeterministicChoice {
	var out []NonDeterministicChoice
	for _, a := range axes {
		if a.Cardinality() > 0 {
			out = append(out, a)
		}
	}
	for _, p := range probAxes {
		if p.Cardinality() > 0 {
			out = append(out, p)
		}
	}
	for _, p := range parAxes {
		if p.Cardinality() > 0 {
			out = append(out, p)
		}
	}
	return out
}

// expandLeaves runs the mixed-radix walk over a flat
// []NonDeterministicChoice. Returns one PlanLeaf per cross-product
// cell. The empty-axes case returns one degenerate leaf so
// callers can iterate uniformly regardless of fan-out status.
func expandLeaves(choices []NonDeterministicChoice) []PlanLeaf {
	if len(choices) == 0 {
		return []PlanLeaf{{Index: 0, Choices: map[string]int{}}}
	}
	total := 1
	for _, c := range choices {
		total *= c.Cardinality()
	}
	leaves := make([]PlanLeaf, total)
	for i := 0; i < total; i++ {
		leaf := PlanLeaf{Index: i, Choices: map[string]int{}}
		rem := i
		for _, c := range choices {
			card := c.Cardinality()
			c.Apply(&leaf, rem%card)
			rem /= card
		}
		leaves[i] = leaf
	}
	return leaves
}
