package star

import (
	"fmt"

	"go.starlark.net/syntax"
)

// assumeSandboxDenylist is the set of Faultbox builtin names that
// assume() predicates may NOT reference (RFC-043 §5.4, §8.7). The
// list mirrors monitorSandboxDenylist in shape — concrete name +
// reason — because the threat model is the same: a predicate runs
// during the test lifecycle and mutating runtime state from it
// races with the body or corrupts the plan-tree decision.
//
// What's intentionally NOT denied:
//   - choose / nondet — predicates legitimately inspect the choices
//     dict via the function argument. The choose() builtin itself
//     isn't called from inside a predicate body, but referencing
//     the identifier in module scope is fine.
//   - Standard Starlark builtins (len, type, etc.) — those don't
//     touch Faultbox state.
var assumeSandboxDenylist = map[string]string{
	// Fault injection — same reasoning as the monitor sandbox.
	"fault":            "fault injection from inside an assume predicate races with the test body",
	"fault_all":        "fault injection from inside an assume predicate races with the test body",
	"fault_start":      "fault injection from inside an assume predicate races with the test body",
	"fault_stop":       "fault injection from inside an assume predicate races with the test body",
	"fault_assumption": "fault declarations belong in the spec body, not in assume predicates",
	"fault_scenario":   "scenario declarations belong in the spec body, not in assume predicates",
	"fault_matrix":     "scenario declarations belong in the spec body, not in assume predicates",

	// Declarations — only valid at spec-load time.
	"service":      "service declarations are spec-load only",
	"interface":    "interface declarations are spec-load only",
	"mock_service": "mock_service declarations are spec-load only",

	// Test runner integration — meaningful only inside a test body.
	"parallel":  "parallel() only meaningful inside the test body",
	"partition": "partition() only meaningful inside the test body",

	// Plan-tree pruning — recursive pruning from inside a predicate
	// would create a cycle (predicate halts → predicate re-runs).
	"halt": "halt() inside an assume predicate would create a recursive pruning cycle",

	// Temporal primitives — would re-enter the runtime.
	"eventually": "registering a new expectation from inside an assume predicate is not supported",
	"always":     "registering a new expectation from inside an assume predicate is not supported",
	"monitor":    "registering a new monitor from inside an assume predicate is not supported",

	// Body-blocking primitives — would deadlock the spec-load thread
	// (top-level assume) or the body-entry path (per-test assume).
	"await_stable": "await_* primitives block the test body and cannot be called from a predicate",
	"await_event":  "await_* primitives block the test body and cannot be called from a predicate",

	// Assertion builtins — predicates signal failure via their bool
	// return value, not via assert_*.
	"assert_true":       "assume predicates signal failure via their return value, not assert_*",
	"assert_eq":         "assume predicates signal failure via their return value, not assert_*",
	"assert_eventually": "assume predicates signal failure via their return value, not assert_*",
	"assert_never":      "assume predicates signal failure via their return value, not assert_*",
	"assert_before":     "assume predicates signal failure via their return value, not assert_*",
}

// validateAssumeLambdasInSource re-parses src and validates every
// lambda body passed to a top-level `assume(...)` call or as an
// element of a `test(..., assume=[...])` list. Mirrors
// validateMonitorLambdasInSource (RFC-041 §8.7) — the existing
// validateSandboxNode walker is reused with a different denylist.
//
// Limitation (documented in the rc2 docs): only lambda bodies are
// statically validated. Named `def` predicates pass through because
// Starlark's AST doesn't surface a def's body through the same
// syntax-tree path during a call-expression walk; calls to a
// denied builtin inside a def slip past the sandbox. This matches
// the monitor predicate limitation.
func validateAssumeLambdasInSource(filename, src string) error {
	if src == "" {
		return nil
	}
	file, err := syntax.Parse(filename, src, 0)
	if err != nil {
		return nil // canonical error comes from ExecFile
	}
	var firstErr error
	syntax.Walk(file, func(n syntax.Node) bool {
		if firstErr != nil {
			return false
		}
		call, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fn.(*syntax.Ident)
		if !ok {
			return true
		}
		switch callee.Name {
		case "assume":
			// Top-level form: validate every positional lambda arg.
			for _, arg := range call.Args {
				if err := validateAssumeNode(arg); err != nil {
					firstErr = err
					return false
				}
			}
		case "test":
			// Per-test form: validate every element of the
			// `assume=[...]` kwarg list.
			for _, arg := range call.Args {
				binop, ok := arg.(*syntax.BinaryExpr)
				if !ok || binop.Op != syntax.EQ {
					continue
				}
				kwName, ok := binop.X.(*syntax.Ident)
				if !ok || kwName.Name != "assume" {
					continue
				}
				list, ok := binop.Y.(*syntax.ListExpr)
				if !ok {
					continue
				}
				for _, item := range list.List {
					if err := validateAssumeNode(item); err != nil {
						firstErr = err
						return false
					}
				}
			}
		}
		return true
	})
	return firstErr
}

// validateAssumeNode walks `root` and rejects identifier
// references that match assumeSandboxDenylist. Tolerant of
// non-lambda inputs (returns nil) so callers don't have to
// type-assert before invoking.
func validateAssumeNode(root syntax.Node) error {
	if root == nil {
		return nil
	}
	var firstErr error
	syntax.Walk(root, func(n syntax.Node) bool {
		if firstErr != nil {
			return false
		}
		if ident, ok := n.(*syntax.Ident); ok {
			if reason, denied := assumeSandboxDenylist[ident.Name]; denied {
				firstErr = fmt.Errorf(
					"assume predicate references disallowed builtin %q at %s: %s",
					ident.Name, ident.NamePos, reason,
				)
				return false
			}
		}
		return true
	})
	return firstErr
}
