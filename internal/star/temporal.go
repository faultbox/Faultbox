package star

import (
	"fmt"
	"sync"

	"go.starlark.net/starlark"
)

// Verdict is the three-valued result of evaluating a temporal expectation.
// PR 4 (this file) defines the type; PR 6 wires it into TestResult.
type Verdict int

const (
	// VerdictPending — the expectation has not yet been decided. For
	// eventually(p) this means "p has not yet been observed true." For
	// always(p) this means "no violation observed, but the window has
	// not yet closed." The test runner waits for Termination to convert
	// any remaining VerdictPending into PASS/FAIL/INCONCLUSIVE per the
	// RFC-041 §5.5 table.
	VerdictPending Verdict = iota
	// VerdictPass — the expectation is satisfied. For eventually(p),
	// p was observed true at some point. For always(p), the window
	// closed without violation.
	VerdictPass
	// VerdictFail — the expectation cannot be satisfied. For always(p),
	// a violating event was observed (failure is immediate; no waiting
	// for Termination). eventually(p) never returns Fail during run —
	// only at Termination via PR 6's lifecycle code.
	VerdictFail
)

func (v Verdict) String() string {
	switch v {
	case VerdictPass:
		return "pass"
	case VerdictFail:
		return "fail"
	case VerdictPending:
		return "pending"
	}
	return "unknown"
}

// TerminationCause indicates which lifecycle condition (RFC-041 §5.5)
// ended a test. Passed to ExpectationVal.Finalize so each expectation
// can produce the right verdict for its cause (e.g. eventually's
// pending → fail under terminate_when but → inconclusive under timeout).
type TerminationCause int

const (
	// TerminationNatural — body returned and all pending expectations
	// already reached a positive terminal state (RFC-041 §5.5(a)).
	TerminationNatural TerminationCause = iota
	// TerminationTerminateWhen — user-declared terminate_when predicate
	// fired (RFC-041 §5.5(b)). Pending eventually → FAIL.
	TerminationTerminateWhen
	// TerminationTimeout — wall-clock test timeout elapsed (RFC-041
	// §5.5(c)). Pending eventually → INCONCLUSIVE.
	TerminationTimeout
	// TerminationImmediateFail — body raised, always violated, monitor
	// fired (RFC-041 §5.5(d)). Pending expectations rolled into the fail.
	TerminationImmediateFail
)

// ExpectationVal is a temporal assertion declared by eventually() or
// always(). It is both a Starlark value (so users can store it in
// variables) and a runnable evaluator (called by the test runner after
// each event and at Termination).
//
// Implementations are concurrency-safe: Evaluate and Finalize may be
// called from the runtime's event-log subscriber goroutine.
type ExpectationVal interface {
	starlark.Value

	// Name returns a short human-readable identifier used in failure
	// messages and the report ("eventually(charge.success)").
	Name() string

	// Evaluate is called after each event arrives during the test. It
	// returns the current verdict and a short reason string for the
	// terminal verdicts. Pending verdicts ignore the reason.
	//
	// thread is a fresh Starlark thread the implementation may use to
	// call the user's predicate; log is the live event log.
	Evaluate(thread *starlark.Thread, log *EventLog) (Verdict, string, error)

	// Finalize is called at Termination. It returns the final verdict
	// per the RFC-041 §5.5 table — eventually's pending → fail under
	// terminate_when, inconclusive under timeout. Always's pending →
	// pass if the window's end-anchor was reached, otherwise inconclusive.
	Finalize(thread *starlark.Thread, log *EventLog, cause TerminationCause) (Verdict, string)
}

// ---------------------------------------------------------------------------
// EventuallyExpectation — RFC-041 §5.1
// ---------------------------------------------------------------------------

// EventuallyExpectation holds the predicate, optional anchor matcher,
// and the "satisfied" flag that flips once the predicate ever returns
// true. Once satisfied, subsequent Evaluate calls are short-circuited.
type EventuallyExpectation struct {
	predicate starlark.Callable
	anchor    *MatcherVal // optional — start watching from first matching event

	mu             sync.Mutex
	satisfied      bool
	anchorReached  bool
}

var _ ExpectationVal = (*EventuallyExpectation)(nil)

func (e *EventuallyExpectation) String() string        { return fmt.Sprintf("<eventually %s>", funcName(e.predicate)) }
func (e *EventuallyExpectation) Type() string          { return "eventually" }
func (e *EventuallyExpectation) Freeze()               {}
func (e *EventuallyExpectation) Truth() starlark.Bool  { return true }
func (e *EventuallyExpectation) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: eventually") }
func (e *EventuallyExpectation) Name() string          { return "eventually(" + funcName(e.predicate) + ")" }

func (e *EventuallyExpectation) Evaluate(thread *starlark.Thread, log *EventLog) (Verdict, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.satisfied {
		return VerdictPass, "", nil
	}
	// If an anchor is set and not yet reached, treat the expectation as
	// not-yet-started rather than evaluating.
	if e.anchor != nil && !e.anchorReached {
		if _, found := log.FirstMatching(e.anchor); !found {
			return VerdictPending, "", nil
		}
		e.anchorReached = true
	}
	res, err := starlark.Call(thread, e.predicate, starlark.Tuple{&TraceVal{log: log}}, nil)
	if err != nil {
		return VerdictFail, "predicate raised: " + err.Error(), err
	}
	if res.Truth() == starlark.True {
		e.satisfied = true
		return VerdictPass, "", nil
	}
	return VerdictPending, "", nil
}

func (e *EventuallyExpectation) Finalize(thread *starlark.Thread, log *EventLog, cause TerminationCause) (Verdict, string) {
	// Final-check: predicate might have become true exactly at termination
	// without having been observed earlier. Per RFC-041 §5.1.
	v, _, err := e.Evaluate(thread, log)
	if err != nil {
		return VerdictFail, "predicate raised at termination: " + err.Error()
	}
	if v == VerdictPass {
		return VerdictPass, ""
	}
	// Pending — verdict depends on which Termination cause fired (§5.5 table).
	switch cause {
	case TerminationTerminateWhen, TerminationImmediateFail:
		return VerdictFail, "eventually(" + funcName(e.predicate) + ") never satisfied; terminate_when fired"
	case TerminationTimeout:
		// Caller maps VerdictPending under timeout → INCONCLUSIVE at the
		// suite level. We surface "pending" rather than fabricating a fail.
		return VerdictPending, "eventually(" + funcName(e.predicate) + ") never satisfied within timeout"
	default:
		// TerminationNatural is impossible with pending eventually (the
		// runner only fires natural completion when every eventually is
		// already satisfied). Treat defensively as inconclusive.
		return VerdictPending, "eventually(" + funcName(e.predicate) + ") still pending at unexpected natural completion"
	}
}

// ---------------------------------------------------------------------------
// AlwaysExpectation — RFC-041 §5.2
// ---------------------------------------------------------------------------

// betweenAnchor is one end of an always() window. It is either a named
// lifecycle anchor ("body_start", "body_end", "stable") or a matcher.
type betweenAnchor struct {
	name    string      // "body_start" | "body_end" | "stable" | "" (when matcher set)
	matcher *MatcherVal // optional
}

// AlwaysExpectation evaluates a predicate after every event and fails
// immediately on the first false result, scoped to a window between
// two anchor events. PR 4 ships immediate failure detection; the
// window-anchor lifecycle integration (string anchors) lands with PR 6.
type AlwaysExpectation struct {
	predicate    starlark.Callable
	betweenStart betweenAnchor
	betweenEnd   betweenAnchor

	mu             sync.Mutex
	violated       bool
	violationMsg   string
	windowStarted  bool
	windowEnded    bool
}

var _ ExpectationVal = (*AlwaysExpectation)(nil)

func (a *AlwaysExpectation) String() string        { return fmt.Sprintf("<always %s>", funcName(a.predicate)) }
func (a *AlwaysExpectation) Type() string          { return "always" }
func (a *AlwaysExpectation) Freeze()               {}
func (a *AlwaysExpectation) Truth() starlark.Bool  { return true }
func (a *AlwaysExpectation) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: always") }
func (a *AlwaysExpectation) Name() string          { return "always(" + funcName(a.predicate) + ")" }

func (a *AlwaysExpectation) Evaluate(thread *starlark.Thread, log *EventLog) (Verdict, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.violated {
		return VerdictFail, a.violationMsg, nil
	}
	// Window-start check: if a matcher anchor exists and hasn't been
	// reached, the window hasn't opened — predicate not yet active.
	if !a.windowStarted {
		if a.betweenStart.matcher != nil {
			if _, found := log.FirstMatching(a.betweenStart.matcher); !found {
				return VerdictPending, "", nil
			}
		}
		// Named "body_start" / "" defaults to immediately active. The
		// runtime sets windowStarted on body entry via SetWindowStarted
		// once PR 6 lands; for now we open the window on first eval.
		a.windowStarted = true
	}
	// Window-end check: if a matcher anchor for the end has fired, the
	// predicate stops being checked.
	if a.betweenEnd.matcher != nil && !a.windowEnded {
		if _, found := log.FirstMatching(a.betweenEnd.matcher); found {
			a.windowEnded = true
		}
	}
	if a.windowEnded {
		return VerdictPass, "", nil
	}
	res, err := starlark.Call(thread, a.predicate, starlark.Tuple{&TraceVal{log: log}}, nil)
	if err != nil {
		a.violated = true
		a.violationMsg = "predicate raised: " + err.Error()
		return VerdictFail, a.violationMsg, err
	}
	if res.Truth() != starlark.True {
		a.violated = true
		a.violationMsg = "always(" + funcName(a.predicate) + ") violated"
		return VerdictFail, a.violationMsg, nil
	}
	return VerdictPending, "", nil
}

func (a *AlwaysExpectation) Finalize(thread *starlark.Thread, log *EventLog, cause TerminationCause) (Verdict, string) {
	v, msg, _ := a.Evaluate(thread, log)
	if v == VerdictFail {
		return VerdictFail, msg
	}
	// Window-end anchor never reached and Termination via timeout → §5.5
	// for always says INCONCLUSIVE; we surface pending and the caller maps.
	if a.betweenEnd.matcher != nil && !a.windowEnded {
		switch cause {
		case TerminationTimeout:
			return VerdictPending, "always(" + funcName(a.predicate) + ") end-anchor never reached within timeout"
		case TerminationTerminateWhen:
			// terminate_when closing the test without the end-anchor: window
			// effectively spans the entire test, predicate held throughout → PASS.
			return VerdictPass, ""
		}
	}
	return VerdictPass, ""
}

// ---------------------------------------------------------------------------
// Builtins: eventually() and always()
// ---------------------------------------------------------------------------

// builtinEventually constructs an EventuallyExpectation.
// Signature: eventually(predicate, anchor=<matcher>)
func (rt *Runtime) builtinEventually(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var predicate starlark.Callable
	var anchorArg starlark.Value = starlark.None
	if err := starlark.UnpackArgs("eventually", args, kwargs,
		"predicate", &predicate,
		"anchor?", &anchorArg,
	); err != nil {
		return nil, err
	}
	exp := &EventuallyExpectation{predicate: predicate}
	if anchorArg != starlark.None {
		m, err := matcherOrPredFromArg(anchorArg)
		if err != nil {
			return nil, fmt.Errorf("eventually(anchor=...): %w", err)
		}
		exp.anchor = m
	}
	rt.registerExpectation(exp)
	return exp, nil
}

// builtinAlways constructs an AlwaysExpectation.
// Signature: always(predicate, between=(start_anchor, end_anchor))
//
// Anchors may be matchers (MatcherVal) or strings ("body_start", "body_end",
// "stable"). String anchors require PR 6 lifecycle wiring to be effective;
// matcher anchors work today.
func (rt *Runtime) builtinAlways(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var predicate starlark.Callable
	var betweenArg starlark.Value = starlark.None
	if err := starlark.UnpackArgs("always", args, kwargs,
		"predicate", &predicate,
		"between?", &betweenArg,
	); err != nil {
		return nil, err
	}
	exp := &AlwaysExpectation{predicate: predicate}
	if betweenArg != starlark.None {
		t, ok := betweenArg.(starlark.Tuple)
		if !ok || len(t) != 2 {
			return nil, fmt.Errorf("always(between=...) must be a tuple of two anchors")
		}
		start, err := parseBetweenAnchor("between[0]", t[0])
		if err != nil {
			return nil, err
		}
		end, err := parseBetweenAnchor("between[1]", t[1])
		if err != nil {
			return nil, err
		}
		exp.betweenStart = start
		exp.betweenEnd = end
	}
	rt.registerExpectation(exp)
	return exp, nil
}

// parseBetweenAnchor converts a Starlark value (string or matcher) into a
// betweenAnchor. Valid string names: "body_start", "body_end", "stable".
func parseBetweenAnchor(name string, v starlark.Value) (betweenAnchor, error) {
	switch val := v.(type) {
	case starlark.String:
		s := string(val)
		switch s {
		case "body_start", "body_end", "stable":
			return betweenAnchor{name: s}, nil
		default:
			return betweenAnchor{}, fmt.Errorf("%s: unknown string anchor %q (valid: body_start, body_end, stable)", name, s)
		}
	case *MatcherVal:
		return betweenAnchor{matcher: val}, nil
	}
	return betweenAnchor{}, fmt.Errorf("%s: anchor must be string or matcher, got %s", name, v.Type())
}

// funcName returns a friendly identifier for a Starlark callable.
func funcName(c starlark.Callable) string {
	if c == nil {
		return "<nil>"
	}
	return c.Name()
}

// ---------------------------------------------------------------------------
// Runtime registration (consumed by PR 6 test lifecycle)
// ---------------------------------------------------------------------------

// registerExpectation adds exp to the per-test expectation list.
// Reset on each RunTest invocation.
func (rt *Runtime) registerExpectation(exp ExpectationVal) {
	rt.temporalMu.Lock()
	defer rt.temporalMu.Unlock()
	rt.temporalExpectations = append(rt.temporalExpectations, exp)
}

// resetExpectations clears the registered list. Called by RunTest.
func (rt *Runtime) resetExpectations() {
	rt.temporalMu.Lock()
	defer rt.temporalMu.Unlock()
	rt.temporalExpectations = nil
}

// snapshotExpectations returns a copy of the registered expectations for
// safe iteration by the test runner (PR 6).
func (rt *Runtime) snapshotExpectations() []ExpectationVal {
	rt.temporalMu.Lock()
	defer rt.temporalMu.Unlock()
	out := make([]ExpectationVal, len(rt.temporalExpectations))
	copy(out, rt.temporalExpectations)
	return out
}
