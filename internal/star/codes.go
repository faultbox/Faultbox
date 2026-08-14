package star

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/resolve"
	"go.starlark.net/syntax"
)

// Machine-readable error codes (RFC-052 Gap 2).
//
// Run-time diagnostics have carried codes since v0.12. Load-time and
// infrastructure failures were prose `fmt.Errorf` strings, and an agent cannot
// branch on prose — it can only pattern-match it, which is the hallucination
// surface this release exists to remove.
//
// # Why a typed error rather than classifying messages
//
// The tempting shortcut is to inspect the error text at the boundary and infer
// a code from it. That reproduces the exact fragility being complained about,
// one layer down: reword an error and the code silently changes, with nothing
// to catch it. So the code is attached where the failure is *raised*, by a type
// the compiler can see.
//
// The consequence is that adoption is incremental. A code exists where the work
// has been done and is absent elsewhere; `Classify` reports that honestly
// rather than guessing. An uncoded error is a gap in a list, which is
// discoverable — a wrongly-coded error is a lie an agent acts on.

// Code is a stable machine-readable identifier for a failure.
//
// Stable is the operative word: these are an API. Renaming one breaks every
// agent that branches on it, so treat additions as cheap and changes as
// breaking.
type Code string

const (
	// Spec-load failures — everything `faultbox check` can find without
	// launching anything.
	CodeSpecSyntax          Code = "SPEC_SYNTAX"
	CodeSpecForbiddenLambda Code = "SPEC_FORBIDDEN_LAMBDA"
	CodeSpecLoadFailed      Code = "SPEC_LOAD_FAILED"
	CodeSpecRecipeNotFound  Code = "SPEC_RECIPE_NOT_FOUND"

	// Infrastructure failures — everything that needs a run.
	CodeHealthcheckTimeout     Code = "HEALTHCHECK_TIMEOUT"
	CodeLaunchFailed           Code = "LAUNCH_FAILED"
	CodeDockerUnavailable      Code = "DOCKER_UNAVAILABLE"
	CodeTraceHostNotRegistered Code = "TRACE_HOST_NOT_REGISTERED"
	CodeFaultNotFilterable     Code = "FAULT_NOT_FILTERABLE"
)

// suggestions maps every code to the agent's next move.
//
// A code without a suggestion is a code the reader has to guess about, which
// defeats the purpose of having codes at all. TestEveryCodeHasASuggestion
// enforces this — the enforcement is the test, not reviewer diligence.
var suggestions = map[Code]string{
	CodeSpecSyntax: "The spec is not valid Starlark. Note the dialect differs from Python: " +
		"no while loops at top level, no list comprehensions with multiple for clauses, " +
		"and positional arguments cannot follow keyword arguments. See docs/starlark-dialect.md.",
	CodeSpecForbiddenLambda: "monitor() update=/check= and assume() predicates run in a sandbox: " +
		"they may not call fault(), await_*, assert_*, or another monitor. Signal failure " +
		"through the return value instead.",
	// Deliberately general. This is the fallback for "parsed and resolved, then
	// failed while executing", which covers unknown keyword arguments, rejected
	// argument values, undeclared service references and removed builtins alike.
	// An earlier version advised checking keyword arguments specifically, which
	// was actively unhelpful when the real problem was a builtin removed three
	// releases ago — the message said one thing and the suggestion another.
	CodeSpecLoadFailed: "The spec parsed but failed while loading. The message above names the " +
		"call that failed; check its arguments against docs/spec-language.md. Unknown keyword " +
		"arguments are rejected rather than ignored, and builtins removed in a past release " +
		"report where their replacement lives.",
	CodeSpecRecipeNotFound: "No such recipe in the embedded stdlib. Run `faultbox recipes list` " +
		"to see what ships, and note the @faultbox/ prefix is required for stdlib loads.",

	CodeHealthcheckTimeout: "The service never became ready. If the healthcheck is tcp(), it only " +
		"proves a port is bound — prefer ready(), which asks the service through its protocol " +
		"plugin using the credentials declared in env=. If it is already ready(), the service " +
		"is genuinely not starting: check its logs and its env.",
	CodeLaunchFailed: "The service could not be started. For binary mode, check the path exists " +
		"and is executable. For container mode, check the image reference and that the image " +
		"can be pulled.",
	CodeFaultNotFilterable: "The fault targets a syscall this service is not intercepting, so it " +
		"could not have fired. Faultbox chooses what to intercept by scanning the spec " +
		"before the run; write the fault inline in the fault() call rather than building " +
		"it in a variable or a helper function, so the scan can see it.",

	CodeDockerUnavailable: "Docker is not reachable. Start the daemon, or on macOS start the " +
		"Lima VM with `make env-start` — container-mode services and every protocol proxy " +
		"depend on it.",
	CodeTraceHostNotRegistered: "watch() needs the host registered once with " +
		"`sudo faultbox setup-trace`, followed by a Docker daemon restart. " +
		"`faultbox setup-trace --check` reports what is installed.",
}

// Suggestion returns the next move for a code, or "" if the code is unknown.
func (c Code) Suggestion() string { return suggestions[c] }

// AllCodes lists every registered code in deterministic order. Used by the
// documentation generator and the gate test.
func AllCodes() []Code {
	out := make([]Code, 0, len(suggestions))
	for c := range suggestions {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CodedError is an error carrying a machine-readable code.
type CodedError struct {
	Code Code
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

// coded wraps err with a code, preserving the wrapped chain.
//
// Returns nil for a nil error so call sites can stay in the
// `return coded(CodeX, doThing())` shape without a nil check.
func coded(c Code, err error) error {
	if err == nil {
		return nil
	}
	return &CodedError{Code: c, Err: err}
}

// codedf wraps a formatted error with a code.
func codedf(c Code, format string, args ...any) error {
	return &CodedError{Code: c, Err: fmt.Errorf(format, args...)}
}

// Classify extracts the code from an error, reporting whether one was found.
//
// Returns false for an uncoded error rather than guessing. That is deliberate:
// a gap in the taxonomy is discoverable and can be filled, while a wrong code is
// something an agent acts on.
func Classify(err error) (Code, bool) {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	return "", false
}

// DiagnosticFor renders an error as a Diagnostic when it carries a code.
//
// Returns nil for uncoded errors — the caller still has the error itself, and
// inventing a diagnostic without a code or suggestion would add ceremony
// without adding information.
func DiagnosticFor(err error) *Diagnostic {
	c, ok := Classify(err)
	if !ok {
		return nil
	}
	return &Diagnostic{
		Level:      "error",
		Code:       string(c),
		Message:    strings.TrimSpace(err.Error()),
		Suggestion: c.Suggestion(),
	}
}

// specLoadCode distinguishes a Starlark syntax error from a spec that parsed
// but failed while executing — a bad kwarg, an undeclared service, a rejected
// value.
//
// This is the one place a code is chosen by inspecting an error, and it is
// narrow on purpose: the distinction is drawn from the error's *type*, never
// from its message text.
//
// Two types mean "structurally wrong, nothing ran":
//
//   - *syntax.Error   — the parser rejected it (unbalanced bracket, bad token)
//   - resolve.ErrorList — the resolver rejected it (positional after named,
//     undefined name, assignment to a global from a function)
//
// Both are one fix category for the author: the file is malformed and no code
// executed. Everything else ExecFile can return is a spec that parsed, resolved,
// and *then* went wrong — a bad kwarg value, a rejected argument — which is a
// materially different thing to go and fix.
func specLoadCode(err error) Code {
	// An inner code wins. Errors raised during load — a missing recipe, a
	// sandbox violation in a loaded module — travel up through ExecFile, and
	// overwriting them here would replace a precise diagnosis with a generic
	// one plus an irrelevant suggestion. Observed: a missing @faultbox/ recipe
	// reported SPEC_LOAD_FAILED and advised checking keyword arguments.
	if c, ok := Classify(err); ok {
		return c
	}

	var se *syntax.Error
	if errors.As(err, &se) {
		return CodeSpecSyntax
	}
	var rl resolve.ErrorList
	if errors.As(err, &rl) {
		return CodeSpecSyntax
	}
	return CodeSpecLoadFailed
}
