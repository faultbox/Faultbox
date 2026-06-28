package star

import (
	"fmt"
	"sync"
	"time"

	"go.starlark.net/starlark"
)

// TestConfig is the per-test declarative metadata produced by the
// RFC-041 §8.6 test(name=, body=, setup=, expect=, timeout=,
// terminate_when=) builtin. Legacy `def test_*()` functions have no
// TestConfig — they run under the simpler synchronous lifecycle.
//
// All callable fields are optional except Body.
//
// PR 6's RunTest reads this struct to:
//   - apply the per-test timeout (Timeout, default 30s when zero)
//   - call Setup before the body (if set)
//   - register Expect with the temporal-expectation list (if set)
//   - watch TerminateWhen on the event log (if set)
type TestConfig struct {
	Body          starlark.Callable
	Setup         starlark.Callable
	Expect        ExpectationVal
	Timeout       time.Duration // 0 → use suite default (30s)
	TerminateWhen ExpectationVal
	// Assumes are RFC-043 §5.4 per-test predicates. RunTest evaluates
	// them at body entry against the current choices dict; the first
	// failing predicate halts the test with Result="halted".
	Assumes []*AssumePredicate
}

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
	case TerminationNatural:
		// Body returned but the eventually never satisfied. RFC §5.5(a)
		// table: "natural completion requires every eventually already
		// satisfied" — so an unsatisfied eventually at body-return is
		// the contradiction case → FAIL. Distinguished from the timeout
		// case (INCONCLUSIVE) because the body had its chance.
		return VerdictFail, "eventually(" + funcName(e.predicate) + ") never satisfied before body returned"
	default:
		return VerdictPending, "eventually(" + funcName(e.predicate) + ") still pending"
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
// two anchor events. Matcher anchors are checked by polling the event
// log on each Evaluate; string anchors ("body_start", "body_end",
// "stable") are signalled by the runtime via SetWindowStarted/Ended
// at lifecycle transitions.
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

// SetWindowStarted is called by the runtime when a lifecycle anchor
// fires. anchor is one of "body_start" | "body_end" | "stable". If the
// expectation's start-anchor matches, the window opens and subsequent
// Evaluate calls run the predicate.
func (a *AlwaysExpectation) SetWindowStarted(anchor string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.betweenStart.name == anchor {
		a.windowStarted = true
	}
}

// SetWindowEnded mirrors SetWindowStarted for the close anchor. Once
// ended, future Evaluate calls short-circuit to VerdictPass — the
// predicate is no longer required to hold past the window.
func (a *AlwaysExpectation) SetWindowEnded(anchor string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.betweenEnd.name == anchor {
		a.windowEnded = true
	}
}

// hasNamedStartAnchor reports whether the expectation's start anchor is
// a named lifecycle anchor (rather than a matcher). Used by the runtime
// to decide whether the default "open immediately" path applies.
func (a *AlwaysExpectation) hasNamedStartAnchor() bool {
	return a.betweenStart.name != "" && a.betweenStart.matcher == nil
}

// hasNamedEndAnchor reports whether the expectation's end anchor is a
// named lifecycle anchor.
func (a *AlwaysExpectation) hasNamedEndAnchor() bool {
	return a.betweenEnd.name != "" && a.betweenEnd.matcher == nil
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
	// Window-start check.
	if !a.windowStarted {
		switch {
		case a.betweenStart.matcher != nil:
			// Matcher anchor: poll the event log.
			if _, found := log.FirstMatching(a.betweenStart.matcher); !found {
				return VerdictPending, "", nil
			}
			a.windowStarted = true
		case a.betweenStart.name == "" || a.betweenStart.name == "body_start":
			// No anchor or explicit body_start: the runtime signals
			// body entry via SetWindowStarted("body_start"). Until
			// that fires (and for legacy tests that never call it),
			// open immediately on first Evaluate — the body has
			// already begun running by the time the subscriber fires
			// the first event.
			a.windowStarted = true
		default:
			// "body_end" / "stable" — wait for the runtime to signal.
			return VerdictPending, "", nil
		}
	}
	// Window-end check.
	if !a.windowEnded && a.betweenEnd.matcher != nil {
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
	a.mu.Lock()
	endPending := !a.windowEnded && (a.betweenEnd.matcher != nil || a.hasNamedEndAnchorLocked())
	// An always() with no closing anchor at all (unbounded `always(p)`, no
	// between=) is a safety property over the whole trace. RFC-049
	// Discrepancy 1: a timeout truncates the trace (LTL₃ prefix), so a
	// never-violated unbounded safety property is INCONCLUSIVE, not PASS — a
	// longer trace could still violate it. At natural completion /
	// terminate_when (LTL_f end-of-trace) it stays a definitive PASS, which
	// the fall-through below preserves.
	unbounded := a.betweenEnd.matcher == nil && a.betweenEnd.name == ""
	a.mu.Unlock()
	if endPending {
		switch cause {
		case TerminationTimeout:
			return VerdictPending, "always(" + funcName(a.predicate) + ") end-anchor never reached within timeout"
		case TerminationTerminateWhen:
			// terminate_when closing the test without the end-anchor: window
			// effectively spans the entire test, predicate held throughout → PASS.
			return VerdictPass, ""
		}
	}
	if unbounded && cause == TerminationTimeout {
		return VerdictPending, "always(" + funcName(a.predicate) + ") held through a truncated (timed-out) prefix — safety not definitively established (RFC-049)"
	}
	return VerdictPass, ""
}

// VacuousWindow reports whether this always() had a real start anchor that
// never fired, so the window never opened and the predicate was never actually
// evaluated. Such a property is vacuously satisfied (PASS is preserved — the
// window may be legitimately untriggered, e.g. between=(error, recovery) in a
// run with no error), but a never-firing start anchor is just as often a
// typo'd / misnamed anchor that would otherwise hide as a silent green. The
// runtime emits a `vacuous_property` warning when this is true so the case
// surfaces in the trace (RFC-049 vacuity resolution). An always() that was
// violated is not vacuous; an unbounded always (no anchor, opens immediately)
// never is.
func (a *AlwaysExpectation) VacuousWindow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	hasStartAnchor := a.betweenStart.matcher != nil ||
		(a.betweenStart.name != "" && a.betweenStart.name != "body_start")
	return hasStartAnchor && !a.windowStarted && !a.violated
}

// hasNamedEndAnchorLocked is the lock-already-held variant of
// hasNamedEndAnchor. The Finalize check above already holds a.mu.
func (a *AlwaysExpectation) hasNamedEndAnchorLocked() bool {
	return a.betweenEnd.name != "" && a.betweenEnd.matcher == nil
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
// test() builtin — RFC-041 §8.6
// ---------------------------------------------------------------------------

// builtinTest declares a test with explicit temporal configuration:
//
//	test(name, body=, setup=, expect=, timeout="30s", terminate_when=,
//	     clock="wall")
//
// Required: name (positional string) + body (callable). Optional:
//   - setup           — called once before body; result discarded
//   - expect          — an eventually(...) or always(...) value;
//                       registered alongside any expectations the body
//                       declares inline so the §5.5 Finalize pass sees
//                       it. Body cannot raise from expect=.
//   - timeout         — duration string ("30s", "1m"). Default 30s.
//   - terminate_when  — an eventually(...) value; when its predicate
//                       holds, Termination fires via (b) §5.5.
//   - clock           — reserved per §8.8. "wall" today;
//                       "virtual" → "requires gVisor" error.
//
// Tests declared via test() coexist with legacy def test_*() functions.
// DiscoverTests includes both. The full test name is "test_<n>" so
// `--test foo` and `--test test_foo` both work from the CLI.
func (rt *Runtime) builtinTest(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	var body starlark.Callable
	var setupArg starlark.Value = starlark.None
	var expectArg starlark.Value = starlark.None
	var twArg starlark.Value = starlark.None
	var assumeArg starlark.Value = starlark.None
	var timeoutStr string
	var clockStr = "wall"
	if err := starlark.UnpackArgs("test", args, kwargs,
		"name", &name,
		"body", &body,
		"setup?", &setupArg,
		"expect?", &expectArg,
		"timeout?", &timeoutStr,
		"terminate_when?", &twArg,
		"assume?", &assumeArg,
		"clock?", &clockStr,
	); err != nil {
		return nil, err
	}
	if err := checkReservedClockKwarg(clockStr); err != nil {
		return nil, fmt.Errorf("test(%q): %w", name, err)
	}
	if name == "" {
		return nil, fmt.Errorf("test() name must be non-empty")
	}

	cfg := &TestConfig{Body: body}
	if setupArg != starlark.None {
		cb, ok := setupArg.(starlark.Callable)
		if !ok {
			return nil, fmt.Errorf("test(%q): setup= must be callable, got %s", name, setupArg.Type())
		}
		cfg.Setup = cb
	}
	if expectArg != starlark.None {
		exp, ok := expectArg.(ExpectationVal)
		if !ok {
			return nil, fmt.Errorf("test(%q): expect= must be eventually(...) or always(...), got %s", name, expectArg.Type())
		}
		cfg.Expect = exp
	}
	if twArg != starlark.None {
		tw, ok := twArg.(ExpectationVal)
		if !ok {
			return nil, fmt.Errorf("test(%q): terminate_when= must be eventually(...) or always(...), got %s", name, twArg.Type())
		}
		cfg.TerminateWhen = tw
	}
	if timeoutStr != "" {
		d, err := parseStarDuration(timeoutStr)
		if err != nil {
			return nil, fmt.Errorf("test(%q): bad timeout=%q: %w", name, timeoutStr, err)
		}
		cfg.Timeout = d
	}
	// RFC-043 §5.4 — per-test assume predicates. Accepts a list of
	// callables (preferred) or bare booleans (constant). RunTest
	// evaluates them at body entry against the current choices dict;
	// the first failure halts the test.
	if assumeArg != starlark.None {
		list, ok := assumeArg.(*starlark.List)
		if !ok {
			return nil, fmt.Errorf("test(%q): assume= must be a list of predicates, got %s", name, assumeArg.Type())
		}
		for i := 0; i < list.Len(); i++ {
			pred, err := assumeFromArg(list.Index(i))
			if err != nil {
				return nil, fmt.Errorf("test(%q): assume[%d]: %w", name, i, err)
			}
			cfg.Assumes = append(cfg.Assumes, pred)
		}
	}

	fullName := "test_" + name
	if _, dup := rt.testConfigs[fullName]; dup {
		return nil, fmt.Errorf("test(%q): a test with this name is already registered", name)
	}
	// Register the config. DiscoverTests will surface the entry as a
	// discoverable test name and register the body into rt.globals
	// (which is nil during the spec-load ExecFile and only populated
	// once execution returns). Following the scenario()/fault_scenario
	// pattern in this file.
	rt.testConfigs[fullName] = cfg
	return starlark.None, nil
}

// ---------------------------------------------------------------------------
// Lifecycle helpers (consumed by RunTest)
// ---------------------------------------------------------------------------

// bodyOutcomeCh is the channel type RunTest uses to await body
// completion. Exported only by signature, not by type alias, to keep
// the surface minimal.
type bodyOutcomeT = struct {
	retVal starlark.Value
	err    error
	// panicVal is set when the body goroutine recovered from a Go-level
	// panic. RunTest uses this to surface the failure as Result="error"
	// so RunAll captures it in SuiteResult.Crash. Empty for normal
	// Starlark errors that the body returned via err.
	panicVal string
}

// waitBodyExit drains a body-outcome channel with a hard deadline.
// Used after TerminationTerminateWhen or TerminationTimeout to give
// the body goroutine a chance to exit cleanly (its await_* primitives
// see the cancelled context and unwind) before the runtime moves on.
//
// If the body does not exit within deadline, returns a zero outcome —
// the body goroutine is leaked. PR 6 ships a 2-second deadline; a
// well-behaved spec yields long before that.
func waitBodyExit(ch <-chan bodyOutcomeT, deadline time.Duration) bodyOutcomeT {
	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case bo := <-ch:
		return bo
	case <-t.C:
		return bodyOutcomeT{}
	}
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

// signalLifecycleAnchor notifies every registered always() expectation
// that a named lifecycle anchor has fired ("body_start", "body_end",
// "stable"). Expectations whose start-anchor matches open their window;
// those whose end-anchor matches close theirs.
//
// This is what makes the string-anchor form `between=("body_start",
// "stable")` actually do anything — without it the anchors are
// silently ignored. The runtime calls this at body entry, body exit,
// and after await_stable returns.
func (rt *Runtime) signalLifecycleAnchor(anchor string) {
	for _, exp := range rt.snapshotExpectations() {
		a, ok := exp.(*AlwaysExpectation)
		if !ok {
			continue
		}
		a.SetWindowStarted(anchor)
		a.SetWindowEnded(anchor)
	}
}
