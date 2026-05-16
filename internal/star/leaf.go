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

// snapshotSpecChoices is called at the end of LoadString/LoadFile.
// Top-level choose() calls have already executed and recorded into
// rt.choices; everything past this point is body-time. The plan
// walker uses the snapshot to reset body-level recordings between
// leaves of the same test without losing the spec preamble.
func (rt *Runtime) snapshotSpecChoices() {
	rt.choicesMu.Lock()
	defer rt.choicesMu.Unlock()
	rt.specChoiceCount = len(rt.choices)
}

// resetBodyChoices truncates rt.choices back to the spec-load-time
// prefix. Called between plan-tree leaves so each leaf's discovery
// run starts from a clean body-level slate. Spec-preamble choices
// stay visible.
func (rt *Runtime) resetBodyChoices() {
	rt.choicesMu.Lock()
	defer rt.choicesMu.Unlock()
	if rt.specChoiceCount < len(rt.choices) {
		rt.choices = rt.choices[:rt.specChoiceCount]
	}
}

// bodyChoices returns the choose() recordings made since the last
// snapshot — i.e. those produced by the most recent body run. Used
// by the plan walker to discover named axes for cross-product
// enumeration.
func (rt *Runtime) bodyChoices() []*ChoiceVal {
	rt.choicesMu.Lock()
	defer rt.choicesMu.Unlock()
	if rt.specChoiceCount >= len(rt.choices) {
		return nil
	}
	out := make([]*ChoiceVal, len(rt.choices)-rt.specChoiceCount)
	copy(out, rt.choices[rt.specChoiceCount:])
	return out
}

// collectNamedAxes returns the named, multi-option choose() axes
// from a recorded-choices slice — these are the axes the plan tree
// fans out over. Anonymous calls (no Name) and single-option
// choices are excluded: anonymous can't be addressed by leaf
// indices, and single-option choices have nothing to vary. Duplicate
// names collapse to the first occurrence; the call site closest to
// the body entry wins, matching how the user reads the spec.
func collectNamedAxes(choices []*ChoiceVal) []*ChoiceVal {
	var axes []*ChoiceVal
	seen := make(map[string]bool)
	for _, c := range choices {
		if c == nil || c.Name == "" || len(c.Options) < 2 {
			continue
		}
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		axes = append(axes, c)
	}
	return axes
}

// enumerateLeaves expands the cross-product of axes into PlanLeaf
// values. With no axes, returns a single degenerate leaf so callers
// can use the same iteration shape for fan-out and non-fan-out
// tests. Leaf 0 is the all-option-0 assignment, which matches what
// the discovery run already executed — callers can reuse that
// run's result for leaf 0 and only re-execute leaves 1..N-1.
//
// Index assignment uses mixed-radix counting: leaf i decodes into
// digit_k = (i / product_{j<k} card_j) mod card_k. Stable across
// runs given the same axes input, which the plan walker guarantees
// by feeding the recorded-order axes through.
func enumerateLeaves(axes []*ChoiceVal) []PlanLeaf {
	if len(axes) == 0 {
		return []PlanLeaf{{Index: 0, Choices: map[string]int{}}}
	}
	total := 1
	for _, a := range axes {
		total *= len(a.Options)
	}
	leaves := make([]PlanLeaf, 0, total)
	for i := 0; i < total; i++ {
		choices := make(map[string]int, len(axes))
		rem := i
		for _, a := range axes {
			n := len(a.Options)
			choices[a.Name] = rem % n
			rem /= n
		}
		leaves = append(leaves, PlanLeaf{Index: i, Choices: choices})
	}
	return leaves
}
