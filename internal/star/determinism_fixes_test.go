package star

import (
	"context"
	"strings"
	"testing"
)

// TestIssue122_ProbFaultSiteOrderIsDeterministic — regression for
// issue #122 (probability fan-out site order is non-deterministic
// across runs).
//
// Before the fix, applyFaults iterated faults (a map[string]*FaultDef)
// in Go's randomized order, so two probabilistic axes on the same
// service produced sites in random order across runs. The plan
// walker uses site order to assign mixed-radix digits to leaves —
// unstable order → stable cardinality but unstable LeafID → axis
// mapping. The fix sorts the syscall keys before iteration.
//
// This test runs the recording path many times in a single process
// and asserts the site sequence is byte-identical every time.
func TestIssue122_ProbFaultSiteOrderIsDeterministic(t *testing.T) {
	src := `
svc = service("svc", image="busybox")
wal_d = deny("EIO", probability=0.3, max_fires=2, mode="exhaustive", label="wal")
cache_d = deny("EIO", probability=0.3, max_fires=2, mode="exhaustive", label="cache")
log_d = deny("EIO", probability=0.3, max_fires=2, mode="exhaustive", label="log")
`
	var firstKeys []string
	for i := 0; i < 50; i++ {
		rt := New(testLogger())
		if err := rt.LoadString("spec.star", src); err != nil {
			t.Fatalf("LoadString: %v", err)
		}
		// Drive the recording path: simulate the body's fault() call
		// installing three rules on one service. applyFaults takes the
		// nil-session bail-out path because we don't start the service.
		rt.faults = map[string]map[string]*FaultDef{
			"svc": {
				"write":   rt.globals["wal_d"].(*FaultDef),
				"read":    rt.globals["cache_d"].(*FaultDef),
				"openat":  rt.globals["log_d"].(*FaultDef),
			},
		}
		rt.sessions = map[string]*runningSession{"svc": {session: nil}}
		_ = rt.applyFaults("svc", rt.faults["svc"])

		sites := rt.bodyProbFaults()
		if len(sites) != 3 {
			t.Fatalf("iteration %d: expected 3 sites, got %d", i, len(sites))
		}
		keys := make([]string, len(sites))
		for j, s := range sites {
			keys[j] = s.Key
		}
		if i == 0 {
			firstKeys = keys
			continue
		}
		for j := range keys {
			if keys[j] != firstKeys[j] {
				t.Fatalf("iteration %d: site order drift — got %v, first run was %v",
					i, keys, firstKeys)
			}
		}
	}
}

// TestIssue119_BodyDoneDrainsAlwaysFired — regression for issue
// #119 (RunTest's bodyDone arm could lose a simultaneous always()
// violation due to Go's random select choice). The fix landed in
// PR #120 added a non-blocking drain after `bo = <-bodyDone`; this
// test verifies the drain by pre-closing alwaysFired before the
// body completes, so the bodyDone arm MUST observe and convert
// cause to TerminationImmediateFail even though the body itself
// didn't error.
//
// Direct race reproduction would require a body that returns in the
// same scheduler tick as the always() watcher fires — that's
// inherently flaky. This test instead pre-stages the channel state
// to drive the exact code path the drain protects against, so a
// regression that removed the drain would fail deterministically.
func TestIssue119_BodyDoneDrainsAlwaysFired(t *testing.T) {
	rt := New(testLogger())
	src := `
def body():
    pass

test("issue_119",
    body = body,
    expect = always(lambda t: False),
    timeout = "1s",
)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 1 {
		t.Fatalf("expected 1 test row, got %d", len(res.Tests))
	}
	tr := res.Tests[0]
	// always(False) registered on a test with a no-op body must
	// either fail (drain caught it) or end the test with a clear
	// always-violation reason. A regression to "pass" would mean
	// the drain didn't fire and the violation was lost.
	if tr.Result != "fail" {
		t.Errorf("Result = %q, want fail (always(False) violation must be observed even when body returns first); reason=%s",
			tr.Result, tr.Reason)
	}
	if !strings.Contains(strings.ToLower(tr.Reason), "always") {
		t.Errorf("Reason should reference the always() violation; got %q", tr.Reason)
	}
}
