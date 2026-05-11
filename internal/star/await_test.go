package star

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// TestAwaitStable_ReturnsAfterQuiescence — with no events emitted and a
// short window, awaitStable returns successfully after the window elapses.
func TestAwaitStable_ReturnsAfterQuiescence(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := awaitStable(ctx, log, 30*time.Millisecond, nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Errorf("returned too early: %v (want ≥ 30ms)", elapsed)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("returned too late: %v (want ≤ 150ms)", elapsed)
	}
}

// TestAwaitStable_ActivityResetsTimer — events fired during the window
// reset the quiescence timer; total wait extends accordingly.
func TestAwaitStable_ActivityResetsTimer(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Fire one event after 20ms — that pushes the 50ms window out.
	go func() {
		time.Sleep(20 * time.Millisecond)
		log.Emit("activity", "svc", nil)
	}()

	start := time.Now()
	if err := awaitStable(ctx, log, 50*time.Millisecond, nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	// Expected: ~20ms (until activity) + 50ms (window after reset) = ~70ms.
	if elapsed < 65*time.Millisecond {
		t.Errorf("returned too early: %v (timer should have reset)", elapsed)
	}
}

// TestAwaitStable_IgnoreFilter — ignored events do NOT reset the timer.
func TestAwaitStable_IgnoreFilter(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Heartbeat-style ignored events every 10ms.
	stopHB := make(chan struct{})
	defer close(stopHB)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopHB:
				return
			case <-ticker.C:
				log.Emit("heartbeat", "svc", nil)
			}
		}
	}()

	ignore := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "heartbeat" }}
	start := time.Now()
	if err := awaitStable(ctx, log, 40*time.Millisecond, ignore); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("ignore should have suppressed heartbeats; took %v", elapsed)
	}
}

// TestAwaitStable_CtxCancelled — when ctx fires before quiescence,
// returns the ctx error so callers can map to INCONCLUSIVE.
func TestAwaitStable_CtxCancelled(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	// Continuous activity prevents quiescence; ctx must win.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				log.Emit("noise", "svc", nil)
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	err := awaitStable(ctx, log, 1*time.Second, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

// TestAwaitStable_RejectsZeroWindow — defensive: callers shouldn't pass 0.
func TestAwaitStable_RejectsZeroWindow(t *testing.T) {
	log := NewEventLog()
	err := awaitStable(context.Background(), log, 0, nil)
	if err == nil {
		t.Error("expected error for zero window")
	}
}

// TestAwaitEvent_EagerReturn — if a matching event is already in the
// log when awaitEvent is called, it returns immediately.
func TestAwaitEvent_EagerReturn(t *testing.T) {
	log := NewEventLog()
	log.Emit("target", "svc", nil)
	log.Emit("noise", "svc", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	m := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "target" }}
	start := time.Now()
	ev, err := awaitEvent(ctx, log, m)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Errorf("eager check should return immediately, took %v", time.Since(start))
	}
	if ev.Type != "target" {
		t.Errorf("expected target event, got %q", ev.Type)
	}
}

// TestAwaitEvent_BlocksThenReturns — when the matching event arrives
// after awaitEvent is called, it returns with that event.
func TestAwaitEvent_BlocksThenReturns(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go func() {
		time.Sleep(30 * time.Millisecond)
		log.Emit("late", "svc", map[string]string{"v": "yes"})
	}()

	m := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "late" }}
	ev, err := awaitEvent(ctx, log, m)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Fields["v"] != "yes" {
		t.Errorf("expected v=yes, got %v", ev.Fields)
	}
}

// TestAwaitEvent_CtxCancelled — if ctx fires before a match, returns
// the ctx error.
func TestAwaitEvent_CtxCancelled(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	m := &MatcherVal{matchFn: func(Event) bool { return false }} // nothing ever matches
	_, err := awaitEvent(ctx, log, m)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

// TestAwaitEvent_IgnoresNonMatching — non-matching events do not
// satisfy the await; the call keeps blocking.
func TestAwaitEvent_IgnoresNonMatching(t *testing.T) {
	log := NewEventLog()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go func() {
		for i := 0; i < 5; i++ {
			log.Emit("noise", "svc", nil)
			time.Sleep(10 * time.Millisecond)
		}
		// Never emits "target".
	}()

	m := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "target" }}
	_, err := awaitEvent(ctx, log, m)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded with no matching events, got %v", err)
	}
}

// TestAwaitEvent_NilMatcherIsError.
func TestAwaitEvent_NilMatcherIsError(t *testing.T) {
	log := NewEventLog()
	_, err := awaitEvent(context.Background(), log, nil)
	if err == nil {
		t.Error("expected error for nil matcher")
	}
}

// TestBuiltinAwaitStable_Smoke — exercise the Starlark wiring with a
// short window and verify it completes.
func TestBuiltinAwaitStable_Smoke(t *testing.T) {
	rt := New(testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rt.setTestContext(ctx)
	defer rt.setTestContext(nil)

	v, err := rt.builtinAwaitStable(nil, nil, nil, []starlark.Tuple{
		{starlark.String("quiescence_window"), starlark.String("30ms")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != starlark.None {
		t.Errorf("await_stable should return None, got %v", v)
	}
}

// TestBuiltinAwaitStable_RejectsVirtualClock — §8.8 reservation.
func TestBuiltinAwaitStable_RejectsVirtualClock(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinAwaitStable(nil, nil, nil, []starlark.Tuple{
		{starlark.String("quiescence_window"), starlark.String("10ms")},
		{starlark.String("clock"), starlark.String("virtual")},
	})
	if err == nil {
		t.Fatal("expected error for clock=virtual")
	}
	if !strings.Contains(err.Error(), "gVisor") {
		t.Errorf("error should mention gVisor migration path, got: %v", err)
	}
}

// TestBuiltinAwaitStable_RejectsUnknownClock — typos should fail loud.
func TestBuiltinAwaitStable_RejectsUnknownClock(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinAwaitStable(nil, nil, nil, []starlark.Tuple{
		{starlark.String("clock"), starlark.String("nonsense")},
	})
	if err == nil {
		t.Error("expected error for clock=nonsense")
	}
}

// TestBuiltinAwaitEvent_ReturnsEventVal — the builtin returns an
// EventVal so it can be chained: event.field, event.happens_before(...).
func TestBuiltinAwaitEvent_ReturnsEventVal(t *testing.T) {
	rt := New(testLogger())
	rt.events.Emit("target", "svc", map[string]string{"k": "v"})
	rt.setTestContext(context.Background())
	defer rt.setTestContext(nil)

	matcher, _ := builtinMatchEvent(nil, nil, nil, []starlark.Tuple{
		{starlark.String("type"), starlark.String("target")},
	})
	v, err := rt.builtinAwaitEvent(nil, nil, starlark.Tuple{matcher}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := v.(*EventVal)
	if !ok {
		t.Fatalf("expected *EventVal, got %T", v)
	}
	if ev.ev.Type != "target" {
		t.Errorf("expected target event, got %q", ev.ev.Type)
	}
}

// TestBuiltinAwaitEvent_RequiresOneArg — calling without an arg errors.
func TestBuiltinAwaitEvent_RequiresOneArg(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinAwaitEvent(nil, nil, starlark.Tuple{}, nil)
	if err == nil {
		t.Error("expected error for await_event with no args")
	}
}

// TestBuiltinAwaitEvent_RejectsVirtualClock — §8.8 reservation.
func TestBuiltinAwaitEvent_RejectsVirtualClock(t *testing.T) {
	rt := New(testLogger())
	matcher, _ := builtinMatchEvent(nil, nil, nil, []starlark.Tuple{
		{starlark.String("type"), starlark.String("x")},
	})
	_, err := rt.builtinAwaitEvent(nil, nil,
		starlark.Tuple{matcher},
		[]starlark.Tuple{{starlark.String("clock"), starlark.String("virtual")}},
	)
	if err == nil {
		t.Error("expected error for clock=virtual on await_event")
	}
}

// TestRuntimeTestContext — sanity that the context plumbing works.
func TestRuntimeTestContext(t *testing.T) {
	rt := New(testLogger())
	if rt.testContext() == nil {
		t.Error("testContext() should never return nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.setTestContext(ctx)
	if rt.testContext() != ctx {
		t.Error("testContext() should return the value set by setTestContext")
	}
	cancel()
	rt.setTestContext(nil)
	if rt.testContext() == nil {
		t.Error("testContext() should fall back to background after clear")
	}
}
