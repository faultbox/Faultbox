package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/bundle"
	"github.com/faultbox/Faultbox/internal/star"
)

// writeTestBundle produces a real .fb archive on disk so the inspect
// helpers exercise the same code path users will.
func writeTestBundle(t *testing.T, faultboxVer string) string {
	t.Helper()
	w := bundle.NewWriter()
	man := bundle.Manifest{
		SchemaVersion:   bundle.SchemaVersion,
		FaultboxVersion: faultboxVer,
		RunID:           "testrun",
		CreatedAt:       "2026-04-22T15:00:00Z",
		Seed:            42,
		SpecRoot:        "smoke.star",
		Tests: []bundle.TestRow{
			{Name: "test_ok", Outcome: "passed", DurationMs: 5},
			{Name: "test_bad", Outcome: "failed", DurationMs: 7},
		},
		Summary: bundle.Summary{Total: 2, Passed: 1, Failed: 1},
	}
	if err := w.AddJSON("manifest.json", man); err != nil {
		t.Fatalf("AddJSON manifest: %v", err)
	}
	if err := w.AddJSON("env.json", bundle.Env{
		FaultboxVersion: faultboxVer,
		HostOS:          "linux",
		HostArch:        "arm64",
	}); err != nil {
		t.Fatalf("AddJSON env: %v", err)
	}
	w.AddFile("trace.json", []byte(`{"version":1}`))
	w.AddFile("replay.sh", bundle.GenerateReplayScript("run.fb", faultboxVer, time.Now()))

	path := filepath.Join(t.TempDir(), "run.fb")
	if err := w.WriteTo(path); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return path
}

func TestInspectSummaryIncludesHeadlineFields(t *testing.T) {
	r, err := bundle.Open(writeTestBundle(t, "0.9.7"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var buf bytes.Buffer
	if rc := printInspectSummary(&buf, r); rc != 0 {
		t.Fatalf("summary rc = %d", rc)
	}
	out := buf.String()

	// Spot-check the fields users rely on when triaging a failed run:
	// which faultbox produced it, with which seed, against which spec,
	// and the test outcomes.
	wants := []string{
		"faultbox 0.9.7", // producer version
		"Seed:            42",
		"Spec root:       smoke.star",
		"1 passed, 1 failed",
		"✓ test_ok",
		"✗ test_bad",
		"manifest.json",
		"replay.sh",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("summary missing %q\n--- output ---\n%s", w, out)
		}
	}
}

func TestInspectVersionBannerSilentOnMatch(t *testing.T) {
	// Same version → no banner at all.
	r, err := bundle.Open(writeTestBundle(t, "0.9.7"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var buf bytes.Buffer
	printVersionBannerIfDrift(&buf, r, "0.9.7")
	if got := buf.String(); got != "" {
		t.Errorf("expected empty banner on match, got: %q", got)
	}
}

func TestInspectVersionBannerOnMinorPatchDrift(t *testing.T) {
	r, err := bundle.Open(writeTestBundle(t, "0.9.7"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var buf bytes.Buffer
	printVersionBannerIfDrift(&buf, r, "0.9.8")
	out := buf.String()
	if !strings.Contains(out, "warn") {
		t.Errorf("expected warning prefix, got: %q", out)
	}
	if !strings.Contains(out, "0.9.7") || !strings.Contains(out, "0.9.8") {
		t.Errorf("banner should name both versions, got: %q", out)
	}
	if strings.Contains(out, "MAJOR") {
		t.Errorf("minor/patch drift should not mention MAJOR, got: %q", out)
	}
}

func TestZeroTrafficSummarySilentWhenAllRulesFired(t *testing.T) {
	// No fault_zero_traffic events → no output. Healthy runs must stay
	// quiet so the terminal signal-to-noise ratio stays high.
	result := &star.SuiteResult{
		Tests: []star.TestResult{
			{Name: "test_ok", Result: "pass", Events: []star.Event{
				{EventType: "fault_applied", Service: "db"},
				{EventType: "fault_removed", Service: "db"},
			}},
		},
	}
	var buf bytes.Buffer
	printZeroTrafficSummary(&buf, result)
	if got := buf.String(); got != "" {
		t.Errorf("expected silent output, got: %q", got)
	}
}

func TestZeroTrafficSummaryListsEveryEvent(t *testing.T) {
	// Aggregate across tests: one line per event, test name prefix,
	// action and op metadata in parentheses. The hint at the end
	// should appear exactly once.
	result := &star.SuiteResult{
		Tests: []star.TestResult{
			{Name: "test_a", Result: "pass", Events: []star.Event{
				{
					EventType: "fault_zero_traffic",
					Service:   "geo",
					Fields:    map[string]string{"syscall": "connect", "action": "deny"},
				},
			}},
			{Name: "test_b", Result: "pass", Events: []star.Event{
				{
					EventType: "fault_zero_traffic",
					Service:   "users",
					Fields:    map[string]string{"syscall": "sendto", "action": "deny", "op": "net_write"},
				},
			}},
		},
	}
	var buf bytes.Buffer
	printZeroTrafficSummary(&buf, result)
	out := buf.String()

	wants := []string{
		"Zero-traffic faults (2)",
		"test_a — geo.connect (deny)",
		"test_b — users.sendto (deny, op=net_write)",
		"scenario may not be exercising",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- output ---\n%s", w, out)
		}
	}
	// The hint should appear exactly once even with multiple events.
	if n := strings.Count(out, "scenario may not be exercising"); n != 1 {
		t.Errorf("hint printed %d times, want 1", n)
	}
}

func TestInspectVersionBannerOnMajorDrift(t *testing.T) {
	r, err := bundle.Open(writeTestBundle(t, "1.2.3"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var buf bytes.Buffer
	printVersionBannerIfDrift(&buf, r, "0.9.7")
	out := buf.String()
	if !strings.Contains(out, "MAJOR") {
		t.Errorf("major drift banner should say MAJOR, got: %q", out)
	}
	if !strings.Contains(out, "refuse") {
		t.Errorf("banner should mention replay will refuse, got: %q", out)
	}
}
