package star

import (
	"fmt"

	"go.starlark.net/starlark"
)

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
