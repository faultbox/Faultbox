package star

import (
	"fmt"

	"go.starlark.net/starlark"
)

// ParallelSite records one parallel() call discovered during a body
// run (RFC-042 §8.8). The plan walker uses these to fan out the plan
// tree across the interleaving axis — one leaf per distinct
// mediated-event ordering when Policy.Kind != "single", a single
// leaf otherwise. Stored on rt.parallelSites with the same
// reset-between-leaves contract as rt.choices and rt.probFaults.
//
// The site captures the policy as authored and the branch count
// (len(args) to parallel()) so the enumerator knows the upper-bound
// interleaving count without re-executing the body. Actual ordering
// enumeration uses the recorded mediated-event log from the
// discovery run (PR 3) plus the policy to decide which orderings
// become leaves.
type ParallelSite struct {
	// Key uniquely identifies this parallel() call across leaves of
	// the same test. Derived from the call site (file:line — see
	// recordParallelSite) so two parallel() calls in the same body
	// stay distinct in the plan tree even when they have the same
	// branch count. Stable across runs given the spec doesn't
	// change.
	Key string
	// Branches is the count of callables passed to parallel(). The
	// enumerator uses this to compute the upper-bound ordering
	// count (factorial(N) for "all", capped subset for "critical"
	// or "n").
	Branches int
	// Policy is the parsed interleavings= kwarg. Drives the plan
	// walker's per-site fan-out cardinality in PR 3.
	Policy InterleavingPolicy
}

// InterleavingPolicy describes how many parallel() interleavings the
// plan tree should fan out (RFC-042 §8.8). The actual leaf count is
// computed by the enumerator given the participating branches; the
// policy is what the spec author opts into.
type InterleavingPolicy struct {
	// Kind discriminates the policy. One of:
	//   "single"   — run one interleaving only (the default —
	//                preserves rc1 single-execution semantics).
	//   "all"      — every distinct ordering of mediated events
	//                across the branches (full Cartesian fan-out).
	//   "critical" — heuristic subset that exercises the boundary
	//                cases (head-to-head, fully-sequential pairs).
	//                Defined in §8.8's "critical" enumeration spec.
	//   "n"        — explicit cap; the first N orderings in plan
	//                order. Useful for cost-bounded exploration.
	Kind string
	// N is meaningful only when Kind == "n". Must be > 0.
	N int
}

// String renders the policy for plan output. Stable across versions
// because the strings appear in plan.json and bundle manifests.
func (p InterleavingPolicy) String() string {
	switch p.Kind {
	case "single":
		return "1"
	case "all":
		return "all"
	case "critical":
		return "critical"
	case "n":
		return fmt.Sprintf("%d", p.N)
	}
	return p.Kind
}

// recordParallelSite appends a parallel() call to the body-discovery
// slice. Sites whose policy is "single" are recorded too — the plan
// walker filters them out when computing axes but keeping them in
// the slice lets the plan-output debug surface "parallel(N
// branches, interleavings=1)" lines for every call so users can see
// which parallel() calls fanned out and which didn't.
//
// Dedup on Key: re-entering the same parallel() statement (e.g. via
// a loop or per-leaf re-execution) keeps the first recording so the
// cardinality doesn't double across leaves. The key is the call
// site's file:line, which is stable across runs given the spec.
func (rt *Runtime) recordParallelSite(site ParallelSite) {
	if site.Key == "" || site.Branches < 2 {
		return
	}
	rt.parallelSitesMu.Lock()
	defer rt.parallelSitesMu.Unlock()
	for _, s := range rt.parallelSites {
		if s.Key == site.Key {
			return
		}
	}
	rt.parallelSites = append(rt.parallelSites, site)
}

// resetBodyParallelSites clears the per-body parallel() recording
// slice. Called alongside resetBodyChoices / resetBodyProbFaults at
// the top of each leaf so the next leaf's discovery starts clean.
func (rt *Runtime) resetBodyParallelSites() {
	rt.parallelSitesMu.Lock()
	defer rt.parallelSitesMu.Unlock()
	rt.parallelSites = nil
}

// bodyParallelSites returns a snapshot of the parallel() sites
// recorded during the most recent body run, in insertion order.
// The plan walker consumes this in PR 3.
func (rt *Runtime) bodyParallelSites() []ParallelSite {
	rt.parallelSitesMu.Lock()
	defer rt.parallelSitesMu.Unlock()
	if len(rt.parallelSites) == 0 {
		return nil
	}
	out := make([]ParallelSite, len(rt.parallelSites))
	copy(out, rt.parallelSites)
	return out
}

// parseInterleavingsKwarg reads RFC-042 §8.8's interleavings= kwarg
// off a parallel() argument list. Returns the resolved policy or an
// error if validation fails. Reserved values ("dpor",
// "sut-internal") are rejected with explicit "future release"
// messages so the kwarg pact stays locked for migrations to RFC-009
// and beyond.
//
// Accepted forms:
//   - int 1                       → InterleavingPolicy{Kind:"single"}
//   - int N (N > 1)               → InterleavingPolicy{Kind:"n", N:N}
//   - string "all"                → InterleavingPolicy{Kind:"all"}
//   - string "critical"           → InterleavingPolicy{Kind:"critical"}
//   - omitted / nil               → InterleavingPolicy{Kind:"single"}
//
// The default-when-omitted is "single" because a spec that ships
// without an interleavings= kwarg today gets one interleaving from
// the existing parallel() semantics. Switching the default to "all"
// would silently fan every parallel() out across the matrix —
// surprising for users on the rc2 migration. Authors opt in.
func parseInterleavingsKwarg(builtinName string, kwargs []starlark.Tuple) (InterleavingPolicy, error) {
	v, ok := starKwarg(kwargs, "interleavings")
	if !ok {
		return InterleavingPolicy{Kind: "single"}, nil
	}
	switch x := v.(type) {
	case starlark.Int:
		n, ok := x.Int64()
		if !ok || n <= 0 {
			return InterleavingPolicy{}, fmt.Errorf("%s(): interleavings= must be a positive integer, got %s", builtinName, v.String())
		}
		if n == 1 {
			return InterleavingPolicy{Kind: "single"}, nil
		}
		return InterleavingPolicy{Kind: "n", N: int(n)}, nil
	case starlark.String:
		s := string(x)
		switch s {
		case "1":
			return InterleavingPolicy{Kind: "single"}, nil
		case "all":
			return InterleavingPolicy{Kind: "all"}, nil
		case "critical":
			return InterleavingPolicy{Kind: "critical"}, nil
		case "dpor":
			return InterleavingPolicy{}, fmt.Errorf("%s(): interleavings=\"dpor\" is reserved for a future Faultbox release (see RFC-009 — independence-relation refinement)", builtinName)
		case "sut-internal":
			return InterleavingPolicy{}, fmt.Errorf("%s(): interleavings=\"sut-internal\" requires L5 (instruction-boundary determinism); not available in this release (see RFC-040 Appendix A)", builtinName)
		}
		return InterleavingPolicy{}, fmt.Errorf("%s(): interleavings= must be 1, an integer N, \"all\", or \"critical\"; got %q", builtinName, s)
	}
	return InterleavingPolicy{}, fmt.Errorf("%s(): interleavings= must be int or string, got %s", builtinName, v.Type())
}
