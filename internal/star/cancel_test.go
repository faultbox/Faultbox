package star

import (
	"context"
	"strings"
	"testing"
)

// An interrupted suite must stop, not race through the remainder.
//
// Before this, RunAll had no cancellation check anywhere in its loop. On
// Ctrl-C or SIGTERM the context was cancelled but the walk continued: every
// remaining test started its services, inherited an already-dead context,
// failed instantly, and was recorded as INCONCLUSIVE. An 18-leaf timing search
// interrupted after one leaf reported "1 passed, 17 inconclusive" — seventeen
// verdicts for tests that never ran.
func TestRunAll_CancelledBeforeStartRunsNothing(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_one():
    assert_true(True, "")

def test_two():
    assert_true(True, "")

def test_three():
    assert_true(True, "")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before the first test gets a chance

	result, err := rt.RunAll(ctx, RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if result.Matched != 3 {
		t.Errorf("Matched = %d, want 3 (discovery is unaffected by cancellation)", result.Matched)
	}
	if result.Aborted != 3 {
		t.Errorf("Aborted = %d, want 3", result.Aborted)
	}
	// The important half: a cancelled run must not manufacture verdicts.
	if result.Pass != 0 || result.Fail != 0 || result.Inconclusive != 0 {
		t.Errorf("cancelled suite produced verdicts: pass=%d fail=%d inconclusive=%d; want all 0",
			result.Pass, result.Fail, result.Inconclusive)
	}
	if len(result.Tests) != 0 {
		t.Errorf("cancelled suite recorded %d test results; want 0", len(result.Tests))
	}
}

// Aborted must stay distinct from Inconclusive. Both mean "no verdict", but
// they need different responses: inconclusive is a property of the test (it
// timed out with pending expectations), aborted is a property of the run
// (someone stopped it). Folding them together would let an interrupted CI job
// look like a flaky suite.
func TestSuiteResult_AbortedIsNotInconclusive(t *testing.T) {
	var s SuiteResult
	s.Aborted = 4
	if s.Inconclusive != 0 {
		t.Fatal("Aborted must not increment Inconclusive")
	}
}

// A spec with no tests must not report an abort just because nothing ran.
func TestRunAll_LiveContextDoesNotAbort(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("spec.star", "def test_ok():\n    assert_true(True, \"\")\n"); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	result, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if result.Aborted != 0 {
		t.Errorf("Aborted = %d on an uninterrupted run, want 0", result.Aborted)
	}
	if result.Pass != 1 {
		t.Errorf("Pass = %d, want 1 — the test should actually have run", result.Pass)
	}
}

// The summary wording is load-bearing: "not run" is the claim, and it must not
// be reachable by any phrasing that implies a measured outcome.
func TestAbortedSummaryWording(t *testing.T) {
	for _, banned := range []string{"inconclusive", "failed", "passed"} {
		const phrase = "not run (interrupted)"
		if strings.Contains(phrase, banned) {
			t.Errorf("abort wording %q contains %q, which implies a verdict", phrase, banned)
		}
	}
}
