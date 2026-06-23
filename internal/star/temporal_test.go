package star

import (
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// makePredicate builds a Starlark callable from a predicate function over
// (*TraceVal) → bool. Used to construct synthetic predicates for testing
// without parsing Starlark source.
func makePredicate(name string, fn func(*TraceVal) bool) starlark.Callable {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 1 {
			return nil, nil
		}
		t, ok := args[0].(*TraceVal)
		if !ok {
			return starlark.False, nil
		}
		return starlark.Bool(fn(t)), nil
	})
}

func TestEventually_Pending(t *testing.T) {
	log := NewEventLog()
	log.Emit("other", "svc", nil)

	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(tv *TraceVal) bool {
			_, ok := tv.log.LastMatching(&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "target" }})
			return ok
		}),
	}
	thread := &starlark.Thread{Name: "test"}
	v, _, err := exp.Evaluate(thread, log)
	if err != nil {
		t.Fatal(err)
	}
	if v != VerdictPending {
		t.Errorf("expected pending, got %v", v)
	}
}

func TestEventually_BecomesSatisfied(t *testing.T) {
	log := NewEventLog()
	log.Emit("other", "svc", nil)

	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(tv *TraceVal) bool {
			_, ok := tv.log.LastMatching(&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "target" }})
			return ok
		}),
	}
	thread := &starlark.Thread{Name: "test"}

	// Not yet satisfied.
	v, _, _ := exp.Evaluate(thread, log)
	if v != VerdictPending {
		t.Fatalf("expected pending before target event, got %v", v)
	}

	// Target arrives — eventually satisfied.
	log.Emit("target", "svc", nil)
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictPass {
		t.Errorf("expected pass after target event, got %v", v)
	}

	// Subsequent evals stay PASS (short-circuit).
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictPass {
		t.Errorf("expected pass on re-eval, got %v", v)
	}
}

func TestEventually_Finalize_PendingWithTimeout(t *testing.T) {
	log := NewEventLog()
	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return false }),
	}
	thread := &starlark.Thread{Name: "test"}

	v, msg := exp.Finalize(thread, log, TerminationTimeout)
	if v != VerdictPending {
		t.Errorf("timeout + never-satisfied → expected pending (→inconclusive), got %v", v)
	}
	if !strings.Contains(msg, "timeout") {
		t.Errorf("message should mention timeout, got %q", msg)
	}
}

func TestEventually_Finalize_PendingWithTerminateWhen(t *testing.T) {
	log := NewEventLog()
	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return false }),
	}
	thread := &starlark.Thread{Name: "test"}

	v, msg := exp.Finalize(thread, log, TerminationTerminateWhen)
	if v != VerdictFail {
		t.Errorf("terminate_when + never-satisfied → expected FAIL, got %v", v)
	}
	if !strings.Contains(msg, "terminate_when") {
		t.Errorf("message should mention terminate_when, got %q", msg)
	}
}

func TestEventually_Finalize_PassEvenIfBecameTrueAtEnd(t *testing.T) {
	log := NewEventLog()
	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(tv *TraceVal) bool {
			return tv.log.Len() > 0
		}),
	}
	thread := &starlark.Thread{Name: "test"}

	// Predicate becomes true exactly at finalize time.
	log.Emit("any", "svc", nil)
	v, _ := exp.Finalize(thread, log, TerminationTimeout)
	if v != VerdictPass {
		t.Errorf("predicate true at finalize → expected pass, got %v", v)
	}
}

func TestEventually_AnchorGate(t *testing.T) {
	log := NewEventLog()
	log.Emit("noise", "svc", nil)

	anchorMatcher := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "start" }}
	exp := &EventuallyExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return true }), // always true
		anchor:    anchorMatcher,
	}
	thread := &starlark.Thread{Name: "test"}

	// Anchor not yet matched — eventually is gated as pending despite the
	// always-true predicate.
	v, _, _ := exp.Evaluate(thread, log)
	if v != VerdictPending {
		t.Fatalf("expected pending before anchor, got %v", v)
	}

	log.Emit("start", "svc", nil)
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictPass {
		t.Errorf("expected pass after anchor, got %v", v)
	}
}

func TestAlways_NoViolation(t *testing.T) {
	log := NewEventLog()
	log.Emit("ok", "svc", nil)

	exp := &AlwaysExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return true }),
	}
	thread := &starlark.Thread{Name: "test"}

	v, _, err := exp.Evaluate(thread, log)
	if err != nil {
		t.Fatal(err)
	}
	if v != VerdictPending {
		t.Errorf("no violation should be pending until termination, got %v", v)
	}

	// Finalize without violation → PASS.
	v, _ = exp.Finalize(thread, log, TerminationNatural)
	if v != VerdictPass {
		t.Errorf("expected pass on natural completion, got %v", v)
	}
}

// TestVerdictOracle_FinalizeMatrix is the RFC-049 step-2/3 oracle: it encodes
// the re-derived LTL₃/LTL_f per-property verdict table as the source of truth
// and diffs every (property-state × termination-cause) cell against what the
// engine's Finalize actually returns. A future divergence in any cell is
// either a latent bug or a docs drift — this matrix is where it surfaces.
//
// Scope: per-property (Finalize-level) verdicts. The test-level aggregation —
// notably "any timeout → INCONCLUSIVE" via the runtime catch-all — is guarded
// at the RunTest level (see lifecycle_test.go TestLifecycle_*Timeout*).
func TestVerdictOracle_FinalizeMatrix(t *testing.T) {
	allCauses := []TerminationCause{
		TerminationNatural, TerminationTerminateWhen, TerminationTimeout, TerminationImmediateFail,
	}
	P := VerdictPass
	F := VerdictFail
	I := VerdictPending // "?" at the property level; the runner maps to INCONCLUSIVE

	// Each scenario builds a fresh expectation already driven into a known
	// state, plus the log to Finalize against. The oracle is the expected
	// verdict per cause.
	type scenario struct {
		name  string
		build func() (ExpectationVal, *EventLog)
		want  map[TerminationCause]Verdict
	}

	truePred := func() starlark.Callable { return makePredicate("p", func(*TraceVal) bool { return true }) }
	falsePred := func() starlark.Callable { return makePredicate("p", func(*TraceVal) bool { return false }) }
	thread := &starlark.Thread{Name: "oracle"}

	scenarios := []scenario{
		{
			// eventually satisfied — co-safety ⊤ latches; PASS under every cause.
			name: "eventually/satisfied",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				log.Emit("ok", "svc", nil)
				e := &EventuallyExpectation{predicate: truePred()}
				_, _, _ = e.Evaluate(thread, log) // latch satisfied
				return e, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: P, TerminationTerminateWhen: P, TerminationTimeout: P, TerminationImmediateFail: P,
			},
		},
		{
			// eventually never satisfied — FAIL at a real end-of-trace
			// (natural / terminate_when / immediate-fail), INCONCLUSIVE only
			// under timeout (more time might have helped).
			name: "eventually/unsatisfied",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				log.Emit("ok", "svc", nil)
				return &EventuallyExpectation{predicate: falsePred()}, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: F, TerminationTerminateWhen: F, TerminationTimeout: I, TerminationImmediateFail: F,
			},
		},
		{
			// unbounded always, never violated — PASS at a real end-of-trace,
			// INCONCLUSIVE under timeout (RFC-049 Discrepancy 1).
			name: "always/unbounded/ok",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				log.Emit("ok", "svc", nil)
				return &AlwaysExpectation{predicate: truePred()}, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: P, TerminationTerminateWhen: P, TerminationTimeout: I, TerminationImmediateFail: P,
			},
		},
		{
			// bounded always whose end-anchor closed before termination — the
			// window is definitively decided; PASS under every cause.
			name: "always/bounded/closed",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				end := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "stop" }}
				a := &AlwaysExpectation{predicate: truePred(), betweenEnd: betweenAnchor{matcher: end}}
				log.Emit("normal", "svc", nil)
				_, _, _ = a.Evaluate(thread, log)
				log.Emit("stop", "svc", nil)
				_, _, _ = a.Evaluate(thread, log) // closes window
				return a, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: P, TerminationTerminateWhen: P, TerminationTimeout: P, TerminationImmediateFail: P,
			},
		},
		{
			// bounded always whose end-anchor never arrived — open interval;
			// INCONCLUSIVE under timeout, PASS when a real end-of-trace closes
			// the test around it.
			name: "always/bounded/open",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				end := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "stop" }}
				a := &AlwaysExpectation{predicate: truePred(), betweenEnd: betweenAnchor{matcher: end}}
				log.Emit("normal", "svc", nil)
				_, _, _ = a.Evaluate(thread, log) // pending, window still open
				return a, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: P, TerminationTerminateWhen: P, TerminationTimeout: I, TerminationImmediateFail: P,
			},
		},
		{
			// any violated always — FAIL under every cause.
			name: "always/violated",
			build: func() (ExpectationVal, *EventLog) {
				log := NewEventLog()
				log.Emit("bad", "svc", nil)
				a := &AlwaysExpectation{predicate: falsePred()}
				_, _, _ = a.Evaluate(thread, log) // latch violation
				return a, log
			},
			want: map[TerminationCause]Verdict{
				TerminationNatural: F, TerminationTerminateWhen: F, TerminationTimeout: F, TerminationImmediateFail: F,
			},
		},
	}

	for _, sc := range scenarios {
		for _, cause := range allCauses {
			exp, log := sc.build()
			got, _ := exp.Finalize(thread, log, cause)
			if want := sc.want[cause]; got != want {
				t.Errorf("oracle mismatch [%s × %v]: got %v, want %v", sc.name, cause, got, want)
			}
		}
	}
}

// TestAlways_Unbounded_TimeoutIsInconclusive is the RFC-049 Discrepancy 1
// regression guard: an unbounded always(p) (no between=) that was never
// violated must finalize to PENDING (→ INCONCLUSIVE) under a timeout — the
// trace is a truncated LTL₃ prefix, so "never violated yet" is not a
// definitive PASS. Before the fix this returned VerdictPass.
func TestAlways_Unbounded_TimeoutIsInconclusive(t *testing.T) {
	log := NewEventLog()
	log.Emit("ok", "svc", nil)

	exp := &AlwaysExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return true }),
	}
	thread := &starlark.Thread{Name: "test"}

	if v, _, _ := exp.Evaluate(thread, log); v != VerdictPending {
		t.Fatalf("precondition: expected pending pre-termination, got %v", v)
	}

	v, msg := exp.Finalize(thread, log, TerminationTimeout)
	if v != VerdictPending {
		t.Errorf("unbounded always at timeout must be PENDING (→INCONCLUSIVE), got %v", v)
	}
	if !strings.Contains(msg, "truncated") {
		t.Errorf("message should explain the truncated-prefix reason, got %q", msg)
	}
}

// TestAlways_Unbounded_DefiniteAtEndOfTrace guards the other side: at a real
// end-of-trace (natural completion or terminate_when — both LTL_f), an
// unbounded never-violated always stays a definitive PASS. Only timeout is
// inconclusive.
func TestAlways_Unbounded_DefiniteAtEndOfTrace(t *testing.T) {
	thread := &starlark.Thread{Name: "test"}
	for _, cause := range []TerminationCause{TerminationNatural, TerminationTerminateWhen} {
		log := NewEventLog()
		log.Emit("ok", "svc", nil)
		exp := &AlwaysExpectation{
			predicate: makePredicate("p", func(*TraceVal) bool { return true }),
		}
		_, _, _ = exp.Evaluate(thread, log)
		if v, _ := exp.Finalize(thread, log, cause); v != VerdictPass {
			t.Errorf("cause %v: unbounded never-violated always must be PASS at end-of-trace, got %v", cause, v)
		}
	}
}

// TestAlways_BoundedClosedWindow_TimeoutStaysPass guards that the Discrepancy 1
// fix did not over-reach: a *bounded* always whose end-anchor closed before
// the timeout has a definitively-decided window and stays PASS at timeout —
// the unbounded-truncation rule must not apply to it.
func TestAlways_BoundedClosedWindow_TimeoutStaysPass(t *testing.T) {
	log := NewEventLog()
	endMatcher := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "stop" }}
	exp := &AlwaysExpectation{
		predicate:  makePredicate("p", func(*TraceVal) bool { return true }),
		betweenEnd: betweenAnchor{matcher: endMatcher},
	}
	thread := &starlark.Thread{Name: "test"}

	log.Emit("normal", "svc", nil)
	_, _, _ = exp.Evaluate(thread, log)
	log.Emit("stop", "svc", nil) // closes the window
	if v, _, _ := exp.Evaluate(thread, log); v != VerdictPass {
		t.Fatalf("precondition: window should close to PASS, got %v", v)
	}

	if v, _ := exp.Finalize(thread, log, TerminationTimeout); v != VerdictPass {
		t.Errorf("bounded always with a closed window must stay PASS at timeout, got %v", v)
	}
}

func TestAlways_ViolationIsImmediate(t *testing.T) {
	log := NewEventLog()
	log.Emit("bad", "svc", nil)

	exp := &AlwaysExpectation{
		predicate: makePredicate("p", func(*TraceVal) bool { return false }),
	}
	thread := &starlark.Thread{Name: "test"}

	v, msg, _ := exp.Evaluate(thread, log)
	if v != VerdictFail {
		t.Errorf("always violation should be immediate fail, got %v", v)
	}
	if !strings.Contains(msg, "violated") {
		t.Errorf("message should mention violated, got %q", msg)
	}

	// Re-eval should stay FAIL (short-circuit).
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictFail {
		t.Errorf("expected stable fail on re-eval, got %v", v)
	}
}

func TestAlways_BetweenMatcherEndCloses(t *testing.T) {
	log := NewEventLog()

	endMatcher := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "stop" }}
	exp := &AlwaysExpectation{
		predicate:  makePredicate("p", func(*TraceVal) bool { return true }),
		betweenEnd: betweenAnchor{matcher: endMatcher},
	}
	thread := &starlark.Thread{Name: "test"}

	log.Emit("normal", "svc", nil)
	v, _, _ := exp.Evaluate(thread, log)
	if v != VerdictPending {
		t.Errorf("expected pending before end-anchor, got %v", v)
	}

	log.Emit("stop", "svc", nil)
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictPass {
		t.Errorf("expected pass after end-anchor closes window, got %v", v)
	}
}

func TestAlways_BetweenStartMatcherGates(t *testing.T) {
	log := NewEventLog()
	log.Emit("bad", "svc", nil) // would violate, but window isn't open

	startMatcher := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "go" }}
	exp := &AlwaysExpectation{
		predicate:    makePredicate("p", func(*TraceVal) bool { return false }),
		betweenStart: betweenAnchor{matcher: startMatcher},
	}
	thread := &starlark.Thread{Name: "test"}

	// Window hasn't opened — predicate not yet evaluated.
	v, _, _ := exp.Evaluate(thread, log)
	if v != VerdictPending {
		t.Errorf("expected pending before window opens, got %v", v)
	}

	// Open the window — predicate now evaluated, returns false → fail.
	log.Emit("go", "svc", nil)
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictFail {
		t.Errorf("expected fail after window opens, got %v", v)
	}
}

func TestAlways_TimeoutWithoutEndAnchor_Inconclusive(t *testing.T) {
	log := NewEventLog()
	log.Emit("ok", "svc", nil)

	endMatcher := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "never" }}
	exp := &AlwaysExpectation{
		predicate:  makePredicate("p", func(*TraceVal) bool { return true }),
		betweenEnd: betweenAnchor{matcher: endMatcher},
	}
	thread := &starlark.Thread{Name: "test"}

	// Eval once so the window starts. No violation.
	exp.Evaluate(thread, log)

	v, _ := exp.Finalize(thread, log, TerminationTimeout)
	if v != VerdictPending {
		t.Errorf("end-anchor never reached + timeout → expected pending, got %v", v)
	}
}

func TestAlways_NamedLifecycleAnchors(t *testing.T) {
	// Named start anchor "body_start" should defer evaluation until
	// SetWindowStarted("body_start") fires; named end anchor "stable"
	// should close the window when SetWindowEnded("stable") fires.
	log := NewEventLog()

	exp := &AlwaysExpectation{
		predicate:    makePredicate("p", func(*TraceVal) bool { return false }),
		betweenStart: betweenAnchor{name: "stable"}, // not body_start: must wait for signal
		betweenEnd:   betweenAnchor{name: "body_end"},
	}
	thread := &starlark.Thread{Name: "test"}

	log.Emit("bad", "svc", nil)
	v, _, _ := exp.Evaluate(thread, log)
	if v != VerdictPending {
		t.Errorf("named start anchor not yet signalled — expected pending, got %v", v)
	}

	exp.SetWindowStarted("stable")
	v, _, _ = exp.Evaluate(thread, log)
	if v != VerdictFail {
		t.Errorf("after window opened, predicate is false → expected fail, got %v", v)
	}
}

func TestAlways_SignalLifecycleAnchor_RuntimeWiring(t *testing.T) {
	rt := New(testLogger())
	// Register an always() with a named start anchor only the
	// runtime can fire.
	exp := &AlwaysExpectation{
		predicate:    makePredicate("p", func(*TraceVal) bool { return true }),
		betweenStart: betweenAnchor{name: "body_start"},
		betweenEnd:   betweenAnchor{name: "body_end"},
	}
	rt.registerExpectation(exp)
	rt.signalLifecycleAnchor("body_start")
	if !exp.windowStarted {
		t.Error("signalLifecycleAnchor(body_start) should open window")
	}
	rt.signalLifecycleAnchor("body_end")
	if !exp.windowEnded {
		t.Error("signalLifecycleAnchor(body_end) should close window")
	}
}

func TestBuiltinDuration_ParsesNanoseconds(t *testing.T) {
	v, err := builtinDuration(nil, nil, starlark.Tuple{starlark.String("200ms")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := v.(starlark.Int).Int64()
	want := int64(200 * time.Millisecond)
	if n != want {
		t.Errorf("duration(200ms) = %d ns, want %d", n, want)
	}
}

func TestBuiltinTest_DuplicateNameErrors(t *testing.T) {
	rt := New(testLogger())
	body := makePredicate("body", func(*TraceVal) bool { return true })
	args := starlark.Tuple{starlark.String("dup"), body}
	if _, err := rt.builtinTest(nil, nil, args, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.builtinTest(nil, nil, args, nil); err == nil {
		t.Error("expected error for duplicate test name")
	}
}

func TestBuiltinEventually_RegistersOnRuntime(t *testing.T) {
	rt := New(testLogger())
	exp, err := rt.builtinEventually(nil, nil,
		starlark.Tuple{makePredicate("p", func(*TraceVal) bool { return true })},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := exp.(*EventuallyExpectation); !ok {
		t.Fatalf("expected EventuallyExpectation, got %T", exp)
	}
	registered := rt.snapshotExpectations()
	if len(registered) != 1 {
		t.Errorf("expected 1 registered expectation, got %d", len(registered))
	}
}

func TestBuiltinEventually_AnchorKwarg(t *testing.T) {
	rt := New(testLogger())
	anchor, _ := builtinMatchEvent(nil, nil, nil, []starlark.Tuple{
		{starlark.String("type"), starlark.String("start")},
	})
	exp, err := rt.builtinEventually(nil, nil,
		starlark.Tuple{makePredicate("p", func(*TraceVal) bool { return true })},
		[]starlark.Tuple{{starlark.String("anchor"), anchor}},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := exp.(*EventuallyExpectation)
	if e.anchor == nil {
		t.Error("anchor should be set")
	}
}

func TestBuiltinAlways_BetweenKwarg(t *testing.T) {
	rt := New(testLogger())
	exp, err := rt.builtinAlways(nil, nil,
		starlark.Tuple{makePredicate("p", func(*TraceVal) bool { return true })},
		[]starlark.Tuple{{
			starlark.String("between"),
			starlark.Tuple{starlark.String("body_start"), starlark.String("body_end")},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	a := exp.(*AlwaysExpectation)
	if a.betweenStart.name != "body_start" || a.betweenEnd.name != "body_end" {
		t.Errorf("between not parsed: start=%+v end=%+v", a.betweenStart, a.betweenEnd)
	}
}

func TestBuiltinAlways_BetweenRejectsBadAnchor(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinAlways(nil, nil,
		starlark.Tuple{makePredicate("p", func(*TraceVal) bool { return true })},
		[]starlark.Tuple{{
			starlark.String("between"),
			starlark.Tuple{starlark.String("nonsense"), starlark.String("body_end")},
		}},
	)
	if err == nil {
		t.Error("expected error for invalid string anchor")
	}
}

func TestBuiltinAlways_BetweenRejectsNonTuple(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinAlways(nil, nil,
		starlark.Tuple{makePredicate("p", func(*TraceVal) bool { return true })},
		[]starlark.Tuple{{
			starlark.String("between"),
			starlark.String("not-a-tuple"),
		}},
	)
	if err == nil {
		t.Error("expected error for non-tuple between=")
	}
}

func TestResetExpectations(t *testing.T) {
	rt := New(testLogger())
	rt.registerExpectation(&EventuallyExpectation{})
	rt.registerExpectation(&AlwaysExpectation{})
	if len(rt.snapshotExpectations()) != 2 {
		t.Fatalf("expected 2 registered, got %d", len(rt.snapshotExpectations()))
	}
	rt.resetExpectations()
	if len(rt.snapshotExpectations()) != 0 {
		t.Errorf("expected 0 after reset, got %d", len(rt.snapshotExpectations()))
	}
}

func TestVerdictString(t *testing.T) {
	cases := []struct {
		v    Verdict
		want string
	}{
		{VerdictPass, "pass"},
		{VerdictFail, "fail"},
		{VerdictPending, "pending"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("Verdict(%d).String() = %q, want %q", tc.v, got, tc.want)
		}
	}
}
