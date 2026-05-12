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
