package star

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

func TestSleepFor_WaitsTheFullDuration(t *testing.T) {
	start := time.Now()
	if err := sleepFor(context.Background(), 40*time.Millisecond); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("returned too early: %v (want >= 40ms)", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("returned too late: %v", elapsed)
	}
}

// The whole point of sleep(): unlike await_stable, a busy event log must not
// shorten OR extend the wait. This is the regression guard for the failure
// measured in docs/design/2026-07-28-timing-exploration-experiment.md, where
// a partition's own events kept await_stable from ever quiescing.
func TestSleepFor_UnaffectedByEventTraffic(t *testing.T) {
	log := NewEventLog()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				log.Emit("noise", "svc", nil)
			}
		}
	}()

	start := time.Now()
	if err := sleepFor(context.Background(), 60*time.Millisecond); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	elapsed := time.Since(start)
	// awaitStable with a 60ms window would never return here: events arrive
	// every 2ms. sleepFor must be indifferent.
	if elapsed > 400*time.Millisecond {
		t.Errorf("event traffic extended the wait: %v (want ~60ms)", elapsed)
	}
}

// Zero is a no-op, not an error — deliberately unlike awaitStable, so that
// choose("gap", ["0ms", "400ms"]) compares two delays instead of aborting
// half the leaves.
func TestSleepFor_ZeroReturnsImmediately(t *testing.T) {
	start := time.Now()
	if err := sleepFor(context.Background(), 0); err != nil {
		t.Fatalf("zero duration must be a no-op, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("zero duration should return immediately, took %v", elapsed)
	}
}

func TestSleepFor_NegativeIsAnError(t *testing.T) {
	err := sleepFor(context.Background(), -1*time.Second)
	if err == nil {
		t.Fatal("negative duration must error")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error should name the problem; got %q", err.Error())
	}
}

func TestSleepFor_CancellationReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := sleepFor(ctx, 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancellation should be prompt, took %v", elapsed)
	}
}

// A sleep longer than the test's remaining budget is refused up front rather
// than run into the deadline. Sleeping into a timeout reports INCONCLUSIVE
// with no hint which two numbers disagreed — the diagnosis that cost 36
// minutes of wall clock to make by hand.
func TestSleepFor_RefusesToOutliveTheTestBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sleepFor(ctx, 10*time.Second)
	if err == nil {
		t.Fatal("expected an error when the sleep exceeds the remaining budget")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("should fail fast, not by running into the deadline; got %v", err)
	}
	for _, want := range []string{"remaining budget", "INCONCLUSIVE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %q", want, err.Error())
		}
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Errorf("should have failed immediately, took %v", elapsed)
	}
}

func TestSleepBuiltin_RejectedAtTopLevel(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `sleep("10ms")`)
	if err == nil {
		t.Fatal("sleep() at module top level must error")
	}
	if !strings.Contains(err.Error(), "inside a test body") {
		t.Errorf("error should say where sleep() is allowed; got %q", err.Error())
	}
}

func TestSleepBuiltin_ParsesArgs(t *testing.T) {
	rt := New(testLogger())
	rt.inTest.Store(true)
	defer rt.inTest.Store(false)

	// Positional and keyword forms both work.
	for _, args := range []struct {
		name   string
		args   starlark.Tuple
		kwargs []starlark.Tuple
	}{
		{"positional", starlark.Tuple{starlark.String("1ms")}, nil},
		{"keyword", nil, []starlark.Tuple{{starlark.String("duration"), starlark.String("1ms")}}},
	} {
		if _, err := rt.builtinSleep(nil, nil, args.args, args.kwargs); err != nil {
			t.Errorf("%s form: %v", args.name, err)
		}
	}
}

func TestSleepBuiltin_BadInputs(t *testing.T) {
	rt := New(testLogger())
	rt.inTest.Store(true)
	defer rt.inTest.Store(false)

	cases := []struct {
		name    string
		args    starlark.Tuple
		kwargs  []starlark.Tuple
		wantSub string
	}{
		{
			name:    "unparseable duration",
			args:    starlark.Tuple{starlark.String("soon")},
			wantSub: "bad duration",
		},
		{
			name:    "negative duration",
			args:    starlark.Tuple{starlark.String("-5s")},
			wantSub: "negative",
		},
		{
			name:    "virtual clock is reserved",
			args:    starlark.Tuple{starlark.String("1ms")},
			kwargs:  []starlark.Tuple{{starlark.String("clock"), starlark.String("virtual")}},
			wantSub: "gVisor",
		},
		{
			name:    "unknown clock",
			args:    starlark.Tuple{starlark.String("1ms")},
			kwargs:  []starlark.Tuple{{starlark.String("clock"), starlark.String("monotonic")}},
			wantSub: "must be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.builtinSleep(nil, nil, tc.args, tc.kwargs)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error should contain %q; got %q", tc.wantSub, err.Error())
			}
		})
	}
}
