package star

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.starlark.net/starlark"
)

// awaitStable blocks the calling goroutine until no event matching the
// "activity" predicate has arrived for the full quiescence window, or
// until ctx is cancelled (RFC-041 §5.3.1).
//
// "Activity" means any event that is NOT ignored by `ignore`. When
// ignore is nil, every emitted event counts as activity and resets the
// quiescence timer. The timer starts the moment awaitStable is called;
// if the system was already quiescent at entry, it returns after one
// full window with no need for any events to fire.
//
// At L1 (RFC-040) the window is wall-clock; at L3 it will be virtual
// clock — the substrate switch is the only difference and lands in a
// future release.
//
// Returns context.Canceled or context.DeadlineExceeded if ctx fires
// before quiescence is observed. Caller maps this to INCONCLUSIVE
// (PR 6 §5.5 verdict table).
func awaitStable(ctx context.Context, log *EventLog, window time.Duration, ignore *MatcherVal) error {
	if log == nil {
		return errors.New("awaitStable: nil event log")
	}
	if window <= 0 {
		return errors.New("awaitStable: window must be > 0")
	}

	// resetCh signals "a non-ignored event arrived; restart the timer."
	// Buffered to 1 so emitters never block waiting for the reader.
	resetCh := make(chan struct{}, 1)

	subID := log.Subscribe(nil, func(ev Event) error {
		if ignore != nil && ignore.Matches(ev) {
			return nil // explicitly tolerated noise — does NOT reset
		}
		// Non-blocking drop: if a reset is already queued, one is
		// enough — the timer will be reset on the next loop iteration.
		select {
		case resetCh <- struct{}{}:
		default:
		}
		return nil
	})
	defer log.Unsubscribe(subID)

	timer := time.NewTimer(window)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resetCh:
			// Stop+drain+reset is the textbook safe pattern for
			// time.Timer; using timer.Reset directly after a fired
			// timer can race with the timer goroutine.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(window)
		case <-timer.C:
			return nil
		}
	}
}

// awaitEvent blocks the calling goroutine until the event log contains
// an event matching m, or until ctx is cancelled (RFC-041 §5.3.2).
//
// Eager check on entry: if a matching event has already been emitted
// when awaitEvent is invoked, it returns immediately with that event.
// Otherwise it subscribes to the log and blocks until either a match
// arrives or ctx fires.
//
// Returns the matching event on success. On cancellation, returns the
// zero Event and the ctx error; caller maps to INCONCLUSIVE.
func awaitEvent(ctx context.Context, log *EventLog, m *MatcherVal) (Event, error) {
	if log == nil {
		return Event{}, errors.New("awaitEvent: nil event log")
	}
	if m == nil {
		return Event{}, errors.New("awaitEvent: nil matcher")
	}

	// Eager check — avoid the subscribe/race round-trip when the
	// answer is already known. RFC-041 §5.3.2: "Returns the matching
	// event ... once the condition holds, allowing chaining."
	if ev, ok := log.FirstMatching(m); ok {
		return ev, nil
	}

	// Buffer 1 so the emitter callback never blocks on a slow reader;
	// only the first matching event matters, subsequent matches are
	// dropped.
	hitCh := make(chan Event, 1)
	var once sync.Once

	subID := log.Subscribe(nil, func(ev Event) error {
		if !m.Matches(ev) {
			return nil
		}
		once.Do(func() {
			// Non-blocking send: if hitCh is already filled (race
			// against ctx cancellation completing the call), drop.
			select {
			case hitCh <- ev:
			default:
			}
		})
		return nil
	})
	defer log.Unsubscribe(subID)

	// Race: an event matching m may have been emitted between the
	// eager check above and the Subscribe call. Re-check now under
	// the subscription so we don't miss it.
	if ev, ok := log.FirstMatching(m); ok {
		return ev, nil
	}

	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case ev := <-hitCh:
		return ev, nil
	}
}

// builtinAwaitStable wires awaitStable into the Starlark surface.
//
// Signature (RFC-041 §5.3.1):
//
//	await_stable(quiescence_window="1s", ignore=None, clock="wall")
//
// - quiescence_window — duration string ("1s", "500ms"). Default "1s".
// - ignore — matcher or callable that returns truthy for events to
//   exclude from activity detection. Default None (every event is
//   activity).
// - clock — reserved kwarg per §8.8. Accepts "wall" silently;
//   "virtual" returns an explicit "requires gVisor (Path C)" error so
//   spec authors get a discoverable migration message.
//
// No own timeout — bounded by the per-test context PR 6 propagates
// here via rt.testCtx (or rt.testContext() if a getter is preferred).
func (rt *Runtime) builtinAwaitStable(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("await_stable() takes only keyword arguments")
	}
	var windowStr string = "1s"
	var ignoreArg starlark.Value = starlark.None
	var clockStr string = "wall"
	if err := starlark.UnpackArgs("await_stable", args, kwargs,
		"quiescence_window?", &windowStr,
		"ignore?", &ignoreArg,
		"clock?", &clockStr,
	); err != nil {
		return nil, err
	}
	if err := checkReservedClockKwarg(clockStr); err != nil {
		return nil, err
	}
	window, err := parseStarDuration(windowStr)
	if err != nil {
		return nil, fmt.Errorf("await_stable() bad quiescence_window: %w", err)
	}
	var ignore *MatcherVal
	if ignoreArg != starlark.None {
		ignore, err = matcherOrPredFromArg(ignoreArg)
		if err != nil {
			return nil, fmt.Errorf("await_stable(ignore=...): %w", err)
		}
	}
	ctx := rt.testContext()
	if err := awaitStable(ctx, rt.events, window, ignore); err != nil {
		return nil, fmt.Errorf("await_stable: %w", err)
	}
	return starlark.None, nil
}

// builtinAwaitEvent wires awaitEvent into the Starlark surface.
//
// Signature (RFC-041 §5.3.2):
//
//	await_event(predicate_or_matcher, clock="wall")
//
// The first positional argument is either a MatcherVal (from match.*)
// or a callable taking (event) → bool. Returns the matching EventVal
// for chaining: ev = await_event(match.event(type="x")); use(ev.field).
func (rt *Runtime) builtinAwaitEvent(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("await_event() takes exactly one positional argument (a matcher or predicate)")
	}
	var clockStr string = "wall"
	if err := starlark.UnpackArgs("await_event", starlark.Tuple{}, kwargs,
		"clock?", &clockStr,
	); err != nil {
		return nil, err
	}
	if err := checkReservedClockKwarg(clockStr); err != nil {
		return nil, err
	}
	m, err := matcherOrPredFromArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("await_event(): %w", err)
	}
	ctx := rt.testContext()
	ev, err := awaitEvent(ctx, rt.events, m)
	if err != nil {
		return nil, fmt.Errorf("await_event: %w", err)
	}
	return newEventVal(ev, rt.events), nil
}

// checkReservedClockKwarg implements the §8.8 reservation rule:
// clock="wall" is accepted today; clock="virtual" returns a clear
// "requires gVisor" error so the syntax is reserved without being
// usable. Any other value is an error so typos surface early.
func checkReservedClockKwarg(v string) error {
	switch v {
	case "", "wall":
		return nil
	case "virtual":
		return errors.New("clock=\"virtual\" requires gVisor (Path C); not available in this release")
	}
	return fmt.Errorf("clock=%q: must be \"wall\" (\"virtual\" reserved for a future release)", v)
}
