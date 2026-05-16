package star

import (
	"github.com/faultbox/Faultbox/internal/engine"
)

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

	// ProbabilityOutcomes maps a fault-rule key to the per-occurrence
	// fired/not-fired vector for this leaf (RFC-042 §8.9). The key
	// is the rule's Label when set, else "<service>:<syscall>". Each
	// vector entry corresponds to one occurrence: vec[0] gates the
	// first time the rule's syscall is intercepted, vec[1] the
	// second, etc. Occurrences past len(vec) fall back to the
	// rule's Mode — exhaustive callers should set max_fires=N so
	// the engine never has to extrapolate.
	//
	// A nil or missing entry means "no leaf-pinning" — the engine
	// uses the seeded RNG (stochastic mode), preserving rc1
	// behavior for specs that don't opt into fan-out.
	ProbabilityOutcomes map[string][]bool
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

// ProbabilityFire returns (fired, true) when this leaf pins the
// `occurrence`-th firing of the rule keyed by `key`. (false, false)
// means the engine should fall back to stochastic (RNG) firing —
// either because the leaf is nil, no vector was recorded for this
// rule, or the occurrence index is past the vector length.
//
// Out-of-range occurrence indices are reported as no-pin rather
// than erroring; the runtime treats them as "exhaustive coverage
// exhausted, fall back to RNG," which matches the RFC §8.9 line 347
// behavior for triggers beyond max_fires.
func (l *PlanLeaf) ProbabilityFire(key string, occurrence int) (bool, bool) {
	if l == nil || key == "" || occurrence < 0 {
		return false, false
	}
	vec, ok := l.ProbabilityOutcomes[key]
	if !ok || occurrence >= len(vec) {
		return false, false
	}
	return vec[occurrence], true
}

// probabilityDecider returns the closure passed into a session's
// SessionConfig.ProbabilityDecider for the given service. The closure
// captures rt by pointer so the engine sees the *current* leaf at
// every consultation — important because runTestFanout swaps
// currentLeaf between leaves without re-launching sessions in the
// shared-runtime tests path.
//
// Key derivation: rule.Label when set, else "<service>:<syscall>".
// The label is the documented stable identifier; spec authors who
// want predictable leaf attribution should set it.
//
// The returned decider is nil-safe with respect to currentLeaf — if
// the leaf is nil (single-leaf execution) or has no ProbabilityOutcomes
// entry for this rule, it reports unpinned and the engine falls
// through to the seeded RNG, matching rc1 behavior exactly.
func (rt *Runtime) probabilityDecider(svcName string) func(rule *engine.FaultRule, occurrence int) (bool, bool) {
	return func(rule *engine.FaultRule, occurrence int) (bool, bool) {
		key := rule.Label
		if key == "" {
			key = svcName + ":" + rule.Syscall
		}
		return rt.currentLeaf.ProbabilityFire(key, occurrence)
	}
}
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

// ProbFaultSite records a probability-fanout-eligible fault rule
// discovered during a body run (RFC-042 §8.9). The plan walker uses
// these to fan out 2^MaxFires leaves per site, each leaf carrying
// the per-occurrence fired/not-fired vector in
// PlanLeaf.ProbabilityOutcomes[Key].
//
// Only rules with mode=exhaustive (the default), probability < 1, and
// max_fires > 0 produce a site. Stochastic and unmodeled rules don't
// fan out — the engine keeps using the seeded RNG for them.
type ProbFaultSite struct {
	Key      string  // rule key matching PlanLeaf.ProbabilityOutcomes
	MaxFires int     // axis cardinality is 2^MaxFires
	Prob     float64 // probability metadata for plan reporting
}

// recordProbFault appends a fault site to the body-discovery slice.
// Idempotent on Key: the same rule installed twice (e.g. across
// syscall families) keeps only the first entry to avoid the cardinality
// doubling. The walker resets this between leaves via
// resetBodyProbFaults.
func (rt *Runtime) recordProbFault(site ProbFaultSite) {
	if site.Key == "" || site.MaxFires <= 0 {
		return
	}
	rt.probFaultsMu.Lock()
	defer rt.probFaultsMu.Unlock()
	for _, s := range rt.probFaults {
		if s.Key == site.Key {
			return
		}
	}
	rt.probFaults = append(rt.probFaults, site)
}

// resetBodyProbFaults clears the per-body probability-fault discovery
// slice. Called alongside resetBodyChoices at the top of each leaf so
// the next leaf's discovery starts clean.
func (rt *Runtime) resetBodyProbFaults() {
	rt.probFaultsMu.Lock()
	defer rt.probFaultsMu.Unlock()
	rt.probFaults = nil
}

// bodyProbFaults returns a snapshot of the probabilistic fault sites
// recorded during the most recent body run.
func (rt *Runtime) bodyProbFaults() []ProbFaultSite {
	rt.probFaultsMu.Lock()
	defer rt.probFaultsMu.Unlock()
	if len(rt.probFaults) == 0 {
		return nil
	}
	out := make([]ProbFaultSite, len(rt.probFaults))
	copy(out, rt.probFaults)
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
// values. With no axes of either kind, returns a single degenerate
// leaf so callers can use the same iteration shape for fan-out and
// non-fan-out tests. Leaf 0 is the all-zero assignment, which
// matches what the discovery run already executed — callers can
// reuse that run's result for leaf 0 and only re-execute leaves
// 1..N-1.
//
// Index assignment uses mixed-radix counting across (choice axes,
// probability fault sites). Each probability site contributes a
// 2^MaxFires factor — every per-occurrence fire/no-fire combination
// becomes a leaf. Stable across runs given the same axes input.
//
// Leaf 0's all-zero ProbabilityOutcomes vector encodes "every
// occurrence fires" because bit 0 = first option which is `true`.
// Choosing `true` for occurrence 0 mirrors the discovery run's
// pre-rc2 RNG fallback wherever the discovery happens to fire.
// Callers shouldn't depend on the exact discovery-vs-leaf-0
// equivalence at this level; instead they treat leaf 0 as a fresh
// re-execution like any other leaf for probability axes.
func enumerateLeaves(axes []*ChoiceVal, probAxes []ProbFaultSite) []PlanLeaf {
	if len(axes) == 0 && len(probAxes) == 0 {
		return []PlanLeaf{{Index: 0, Choices: map[string]int{}}}
	}
	total := 1
	for _, a := range axes {
		total *= len(a.Options)
	}
	for _, p := range probAxes {
		total *= 1 << p.MaxFires
	}
	leaves := make([]PlanLeaf, 0, total)
	for i := 0; i < total; i++ {
		choices := make(map[string]int, len(axes))
		probs := make(map[string][]bool, len(probAxes))
		rem := i
		for _, a := range axes {
			n := len(a.Options)
			choices[a.Name] = rem % n
			rem /= n
		}
		for _, p := range probAxes {
			card := 1 << p.MaxFires
			digit := rem % card
			rem /= card
			vec := make([]bool, p.MaxFires)
			for b := 0; b < p.MaxFires; b++ {
				vec[b] = (digit>>b)&1 == 1
			}
			probs[p.Key] = vec
		}
		leaf := PlanLeaf{Index: i, Choices: choices}
		if len(probs) > 0 {
			leaf.ProbabilityOutcomes = probs
		}
		leaves = append(leaves, leaf)
	}
	return leaves
}
