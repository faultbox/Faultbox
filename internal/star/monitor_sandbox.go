package star

import (
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// monitorSandboxDenylist is the set of Faultbox builtin names that
// monitor update/check lambdas may NOT reference (RFC-041 §5.4, §8.7).
// The list intentionally enumerates concrete builtins rather than using
// an allowlist so spec authors can use ordinary Starlark identifiers,
// Python-style helpers, and module-scoped constants from within a
// monitor predicate — only Faultbox-specific calls that would mutate
// runtime state, recurse, or block the test thread are rejected.
//
// Each entry maps the disallowed name to a short reason rendered in
// the spec-load error message so the user knows WHY this name is
// forbidden, not just THAT it is.
var monitorSandboxDenylist = map[string]string{
	// Fault injection — mutates runtime state and races with the test body.
	"fault":            "fault injection from inside a monitor races with the test body",
	"fault_all":        "fault injection from inside a monitor races with the test body",
	"fault_start":      "fault injection from inside a monitor races with the test body",
	"fault_stop":       "fault injection from inside a monitor races with the test body",
	"fault_assumption": "fault declarations belong in the spec body, not in monitors",
	"fault_scenario":   "scenario declarations belong in the spec body, not in monitors",
	"fault_matrix":     "scenario declarations belong in the spec body, not in monitors",

	// Declarations — only valid at spec-load time, not at monitor runtime.
	"service":      "service declarations are spec-load only",
	"interface":    "interface declarations are spec-load only",
	"mock_service": "mock_service declarations are spec-load only",

	// Assertion builtins — drive test results from the body, not from monitors.
	// A monitor's own pass/fail comes from its `check=` callback.
	"assert_true":       "monitors must signal failure via check=, not assert_*",
	"assert_eq":         "monitors must signal failure via check=, not assert_*",
	"assert_eventually": "monitors must signal failure via check=, not assert_*",
	"assert_never":      "monitors must signal failure via check=, not assert_*",
	"assert_before":     "monitors must signal failure via check=, not assert_*",

	// Test runner integration — meaningful only inside a test body.
	"parallel":  "parallel() only meaningful inside the test body",
	"partition": "partition() only meaningful inside the test body",
	"nondet":    "nondet() only meaningful inside the test body",
	"scenario":  "scenario() is a spec-body declaration",

	// Temporal primitives — recursive registration would re-enter the runtime.
	"eventually": "registering a new expectation from inside a monitor is not supported",
	"always":     "registering a new expectation from inside a monitor is not supported",
	"monitor":    "registering a new monitor from inside a monitor is not supported",

	// Body-blocking primitives (PR 5) — would deadlock the event-log subscriber.
	"await_stable": "await_* primitives block the test body and cannot be called from a monitor",
	"await_event":  "await_* primitives block the test body and cannot be called from a monitor",

	// Determinism / trace family — config or runtime-mutating.
	"determinism": "determinism() is a spec-load declaration",
	"trace":       "trace() is a fault primitive; the monitor receives its own trace via the event/state args",
	"trace_start": "trace_*() is a test-body primitive",
	"trace_stop":  "trace_*() is a test-body primitive",
	"events":      "events() is a test-body query; the monitor receives the matching event directly",
}

// newSandboxThread returns a starlark.Thread tagged for monitor evaluation.
// The Name uses a "monitor:<id>" prefix so panics, prints, and call-graph
// debuggers can attribute work to the originating monitor.
//
// The thread does not itself restrict what builtins are callable — that
// guarantee comes from spec-load static validation (validateSandboxNode).
// Using a fresh thread per Evaluate call ensures monitors cannot leak
// per-call state through thread-local data.
func newSandboxThread(monitorID string) *starlark.Thread {
	return &starlark.Thread{Name: "monitor:" + monitorID}
}

// validateSandboxNode walks the AST under root and returns an error on
// the first identifier reference that matches monitorSandboxDenylist.
// The error names the identifier, the reason it is forbidden, and the
// source line so the user can locate the offending call quickly.
//
// root must be a function-like node — a *syntax.LambdaExpr or
// *syntax.DefStmt — but the walker tolerates any starlark AST Node so
// callers can pass an entire CallExpr.kwarg value without first
// type-asserting.
func validateSandboxNode(monitorName string, root syntax.Node) error {
	if root == nil {
		return nil
	}
	var firstErr error
	syntax.Walk(root, func(n syntax.Node) bool {
		if firstErr != nil {
			return false // stop walk
		}
		// We care about plain identifier references — function-call
		// callees and right-hand-side reads. Skip the LHS of
		// assignments and parameter names (those are local bindings
		// the user introduces themselves).
		switch v := n.(type) {
		case *syntax.Ident:
			if reason, denied := monitorSandboxDenylist[v.Name]; denied {
				firstErr = fmt.Errorf(
					"monitor %q references disallowed builtin %q at %s: %s",
					monitorName, v.Name, v.NamePos, reason,
				)
				return false
			}
		case *syntax.AssignStmt:
			// Skip the LHS so binding a denylisted name to a local
			// (which would shadow, not reference) is permitted —
			// e.g. `fault = 42` inside the lambda body. Visit the RHS
			// explicitly via the walk by returning true; the walker
			// then descends into both sides, but the LHS Ident
			// referenced an assignment target, not a name lookup.
			// Starlark resolves `fault = 42` as a local binding, so
			// any subsequent `fault` reference is local — already
			// fine. We let the default walk handle it; this case
			// exists only for symmetry / future tightening.
			_ = v
		}
		return true // continue walk
	})
	return firstErr
}

// validateMonitorLambdasInSource re-parses src as a Starlark file,
// finds every top-level `monitor(...)` call, and validates the
// `update=` and `check=` keyword arguments against
// monitorSandboxDenylist. Returns the first error found (or nil).
//
// This runs at spec load (PR 3 wires it into LoadString/LoadFile),
// before any test executes, so the "monitor uses fault()" failure
// mode surfaces synchronously with the spec error rather than as a
// runtime racing panic deep inside the event-log subscriber.
//
// Parsing is intentionally separate from starlark.ExecFile: ExecFile
// retains the compiled bytecode but discards the syntax tree. Parsing
// here gives us positional information for clean error messages and
// access to the lambda body for AST walking. The duplicate parse is
// cheap (microseconds even for large specs) and runs once per load.
func validateMonitorLambdasInSource(filename, src string) error {
	if src == "" {
		return nil
	}
	file, err := syntax.Parse(filename, src, 0)
	if err != nil {
		// Don't surface parse errors here — let starlark.ExecFile
		// produce the canonical user-facing parse error message.
		return nil
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
		if !ok || callee.Name != "monitor" {
			return true
		}
		monitorName := monitorCallName(call)
		for _, arg := range call.Args {
			binop, ok := arg.(*syntax.BinaryExpr)
			if !ok || binop.Op != syntax.EQ {
				continue
			}
			kwName, ok := binop.X.(*syntax.Ident)
			if !ok {
				continue
			}
			if kwName.Name != "update" && kwName.Name != "check" {
				continue
			}
			if err := validateSandboxNode(monitorName, binop.Y); err != nil {
				firstErr = err
				return false
			}
		}
		return true
	})
	return firstErr
}

// monitorCallName extracts a friendly name for a monitor() call so
// error messages can cite it. Looks for the first positional string
// argument (the conventional name slot) or falls back to "<unnamed>".
func monitorCallName(call *syntax.CallExpr) string {
	for _, arg := range call.Args {
		if lit, ok := arg.(*syntax.Literal); ok {
			if s, ok := lit.Value.(string); ok {
				return s
			}
		}
		// kwarg form name="..."
		if binop, ok := arg.(*syntax.BinaryExpr); ok && binop.Op == syntax.EQ {
			if id, ok := binop.X.(*syntax.Ident); ok && id.Name == "name" {
				if lit, ok := binop.Y.(*syntax.Literal); ok {
					if s, ok := lit.Value.(string); ok {
						return s
					}
				}
			}
		}
	}
	return "<unnamed>"
}

// MonitorSandboxDenylistDescribe returns a sorted, human-readable
// summary of the denylist, suitable for embedding in CLI help output
// or doc generation. Exported so docs/temporal.md and the spec-language
// reference can include the canonical list without duplicating it.
func MonitorSandboxDenylistDescribe() string {
	names := make([]string, 0, len(monitorSandboxDenylist))
	for k := range monitorSandboxDenylist {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "  %-20s — %s\n", n, monitorSandboxDenylist[n])
	}
	return b.String()
}
