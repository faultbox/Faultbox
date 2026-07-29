package star

import (
	"errors"
	"strings"
	"testing"
)

// The honesty guards (RFC-056 M4).
//
// Every one of these turns a run that would otherwise report a confident PASS
// into a failure. That is the point: the canonical watch() assertion is
// negative — "this service never wrote outside its data directory" — and a
// negative assertion is only as good as the completeness of what was observed.
// Each guard covers a way the observation can be incomplete while looking
// exactly like "the service did no such I/O".
//
// Without them the feature ships the failure mode v0.14.0 withdrew watch() to
// avoid, which is why the plan calls M4 non-optional.

// gvisorRT builds a runtime with filesystem observation nominally enabled, so
// the guards are reachable.
func gvisorRT(t *testing.T) *Runtime {
	t.Helper()
	rt := New(testLogger())
	if err := rt.LoadString("spec.star", "determinism(runtime = \"gvisor\")\n"); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	rt.specUsesWatch = true
	return rt
}

func TestGuard_NeverStarted(t *testing.T) {
	rt := gvisorRT(t)
	got := rt.fsObservationFailure()
	if got == "" {
		t.Fatal("a watch with no observation started must not be trusted")
	}
	if !strings.Contains(got, "never started") {
		t.Errorf("reason should say observation never started: %q", got)
	}
}

// A sandbox that never connected means nothing was ever reported. The most
// likely cause is a container launched under plain runsc rather than the
// registered trace runtime — it starts perfectly well and reports nothing, so
// the message has to name that.
func TestGuard_NoSandboxConnected(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.started = true
	rt.fsObs.connected = false

	got := rt.fsObservationFailure()
	if got == "" {
		t.Fatal("a watch where no sandbox connected must fail")
	}
	for _, want := range []string{"no sandbox ever connected", "faultbox-trace", "setup-trace --check"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason should mention %q: %q", want, got)
		}
	}
	if strings.Contains(got, "running under runsc") {
		t.Error("reason still names bare runsc; a container under plain runsc " +
			"starts fine and observes nothing, so that advice would mislead")
	}
}

func TestGuard_DecodeError(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.started = true
	rt.fsObs.connected = true
	rt.fsObs.decodeErr = errors.New("bad wire framing")

	got := rt.fsObservationFailure()
	if !strings.Contains(got, "incomplete") || !strings.Contains(got, "bad wire framing") {
		t.Errorf("reason should surface the decode error and call the trace incomplete: %q", got)
	}
}

// The M0b finding. Dropped points make the observation a subset of what
// happened, and a negative assertion cannot be proven from a subset.
func TestGuard_UnattributedPointsFail(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.started = true
	rt.fsObs.connected = true
	rt.fsObs.unattributed = 42

	got := rt.fsObservationFailure()
	if got == "" {
		t.Fatal("points that matched no service were discarded; that must fail")
	}
	if !strings.Contains(got, "42") {
		t.Errorf("reason should say how many were discarded: %q", got)
	}
	if !strings.Contains(got, "discarded") {
		t.Errorf("reason should say they were discarded, not merely unmatched: %q", got)
	}
}

// A clean run must produce no reason at all — otherwise every watch() fails
// and the guards are worse than useless.
func TestGuard_CleanRunPasses(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.started = true
	rt.fsObs.connected = true

	if got := rt.fsObservationFailure(); got != "" {
		t.Errorf("a healthy observation must produce no failure reason, got: %q", got)
	}
}

// Guards must not fire when the spec never asked for observation. A spec on
// the default runtime that happens to mention watch() in a comment should not
// be failed by them.
func TestGuard_SilentWhenObservationNotEnabled(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("spec.star", "# no determinism() call\n"); err != nil {
		t.Fatal(err)
	}
	if got := rt.fsObservationFailure(); got != "" {
		t.Errorf("guards must be silent when observation was never enabled, got: %q", got)
	}
}

// The order matters: a run that both failed to connect AND discarded points
// should report the connection problem, because that is the cause and the
// other is its symptom.
func TestGuard_ReportsRootCauseFirst(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.started = true
	rt.fsObs.connected = false
	rt.fsObs.unattributed = 7

	got := rt.fsObservationFailure()
	if !strings.Contains(got, "no sandbox ever connected") {
		t.Errorf("should report the connection failure, not the downstream symptom: %q", got)
	}
}

// The attribution fallback: with exactly one observed sandbox there is no
// ambiguity about who did the work, so a container-ID mismatch must not throw
// the point away. This regressed silently in M3 when the map it reads stopped
// being populated.
func TestSoleTracedServiceUsesTheContainerMap(t *testing.T) {
	rt := gvisorRT(t)
	rt.fsObs.containers = map[string]string{"abc123": "db"}

	if got := rt.soleTracedService(); got != "db" {
		t.Errorf("sole traced service = %q, want \"db\"; the fallback reads the wrong map", got)
	}

	// Two sandboxes: ambiguous, so no fallback.
	rt.fsObs.containers["def456"] = "api"
	if got := rt.soleTracedService(); got != "" {
		t.Errorf("with two sandboxes the fallback must not guess, got %q", got)
	}
}

// The M0b guard, tested directly. Reading a real drop count needs a live
// SOCK_SEQPACKET sink and so is Linux-only; this finding is too central to be
// exercised on one platform.
func TestGuard_DroppedPointsFail(t *testing.T) {
	if got := droppedFailure(0); got != "" {
		t.Errorf("zero drops is a clean run, got: %q", got)
	}
	if got := droppedFailure(-1); got != "" {
		t.Errorf("a negative count must not fail a run, got: %q", got)
	}

	got := droppedFailure(1488)
	if got == "" {
		t.Fatal("dropped points must invalidate the watch")
	}
	for _, want := range []string{
		"1488",             // how many
		"subset",           // why it matters
		"could be the one", // what the risk actually is
		"narrow files=",    // what to do about it
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reason should contain %q: %q", want, got)
		}
	}
	// Must not read as a spec bug: the author did nothing wrong.
	if strings.Contains(got, "invalid") || strings.Contains(got, "error in") {
		t.Errorf("reason should not blame the spec: %q", got)
	}
}
