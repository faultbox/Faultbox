package star

import (
	"context"
	"fmt"
	"time"

	"go.starlark.net/starlark"
)

// sleep() — a wall-clock wait that does not care what the event log is doing.
//
// Faultbox already had two ways to wait, and both are conditional on the SUT:
// await_stable() returns when the event log goes quiet, await_event() when a
// matching event arrives. Neither can express "hold this fault for 400ms",
// and await_stable fails at it in the one situation the wait is for.
//
// A fault under test generates the events that prevent quiescence. Measured on
// a 3-node hashicorp/raft cluster with a partition installed: 6681 events in a
// single three-minute window, longest quiet gap 338ms. Every dropped packet
// and every failed heartbeat resets the quiescence timer, so an
// await_stable(quiescence_window="900ms") could never return — it blocked
// until the per-test deadline and the leaf reported INCONCLUSIVE. Twelve of
// eighteen exploration leaves died that way, burning 36 minutes to produce no
// signal. The longer the delay asked for, the more certain it never arrives,
// which is exactly inverted from what a timing knob needs.
//
// ignore= does not rescue it: the noise is the SUT's own stdout, which is also
// what a spec legitimately waits on.
//
// Full measurement in docs/design/2026-07-28-timing-exploration-experiment.md.

// sleepFor blocks for d, or until ctx is cancelled.
//
// A zero duration returns immediately rather than erroring. This is deliberate
// and differs from awaitStable, which rejects a non-positive window: the point
// of sleep() is to be a choose() axis, and "no delay" is the natural baseline
// for such an axis. `choose("gap", ["0ms", "400ms", "1200ms"])` should compare
// three delays, not silently abort a third of the leaves.
func sleepFor(ctx context.Context, d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("duration must not be negative, got %s", d)
	}
	if d == 0 {
		return nil
	}

	// Fail fast rather than sleeping into the test deadline. A spec that asks
	// to wait longer than the test can possibly run should be told which two
	// numbers disagree, not spend the whole budget and report a bare timeout —
	// that is precisely the diagnosis that cost 36 minutes to make by hand.
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); d > remaining {
			return fmt.Errorf(
				"sleep(%s) is longer than the test's remaining budget (%s); "+
					"the test would time out mid-wait and report INCONCLUSIVE rather than fail",
				d, remaining.Round(time.Millisecond))
		}
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// builtinSleep implements sleep(duration, clock="wall").
//
// Signature mirrors await_stable's: a duration string plus the reserved
// clock= kwarg, so the two read alike at a call site and the eventual virtual
// clock (RFC-040 L3) lands on both at once.
//
// No event is emitted. A sleep is supervisor-side, not something the SUT did,
// and emitting one would reset the quiescence timer of any await_stable in a
// parallel() branch — reintroducing, from the inside, the interference this
// primitive exists to escape.
func (rt *Runtime) builtinSleep(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if !rt.inTest.Load() {
		return nil, fmt.Errorf("sleep() may only be called inside a test body; " +
			"got call at module top level (or inside setup=)")
	}
	var durStr string
	clockStr := "wall"
	if err := starlark.UnpackArgs("sleep", args, kwargs,
		"duration", &durStr,
		"clock?", &clockStr,
	); err != nil {
		return nil, err
	}
	if err := checkReservedClockKwarg(clockStr); err != nil {
		return nil, err
	}
	d, err := parseStarDuration(durStr)
	if err != nil {
		return nil, fmt.Errorf("sleep() bad duration %q: %w", durStr, err)
	}
	if err := sleepFor(rt.testContext(), d); err != nil {
		return nil, fmt.Errorf("sleep: %w", err)
	}
	return starlark.None, nil
}
