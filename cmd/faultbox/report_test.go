package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReportCmdWritesHTML is the happy-path integration test: point
// reportCmd at a bundle on disk, check that the resulting file is a
// valid self-contained HTML report with the expected surface markers.
func TestReportCmdWritesHTML(t *testing.T) {
	bundlePath := writeTestBundle(t, "0.11.0")
	outPath := filepath.Join(t.TempDir(), "report.html")

	rc := reportCmd([]string{bundlePath, "--output", outPath})
	if rc != 0 {
		t.Fatalf("reportCmd rc = %d", rc)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	got := string(data)

	// Shell markers must appear verbatim. Bundle contents (test names,
	// run id) live inside the gzip+base64 data block per RFC-031 and
	// are checked separately by internal/report tests.
	wants := []string{
		"<!DOCTYPE html>",
		`id="faultbox-data-gz"`,
		`type="application/octet-stream"`,
		`data-encoding="gzip+base64"`,
		":root {",             // CSS
		"window.__FAULTBOX__", // app.js writes this after parse
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("report missing %q", w)
		}
	}
}

// TestReportCmdSummaryFlag exercises the v0.12 --summary mode end-to-
// end through the CLI. Output must be smaller than the default mode
// against the same bundle and must mark itself as summary in the
// data-mode attribute.
func TestReportCmdSummaryFlag(t *testing.T) {
	bundlePath := writeTestBundle(t, "0.11.0")
	full := filepath.Join(t.TempDir(), "full.html")
	summary := filepath.Join(t.TempDir(), "summary.html")

	if rc := reportCmd([]string{bundlePath, "--output", full}); rc != 0 {
		t.Fatalf("full reportCmd rc = %d", rc)
	}
	if rc := reportCmd([]string{bundlePath, "--summary", "--output", summary}); rc != 0 {
		t.Fatalf("summary reportCmd rc = %d", rc)
	}

	fb, _ := os.ReadFile(full)
	sb, _ := os.ReadFile(summary)
	if len(sb) >= len(fb) {
		t.Errorf("summary (%d) not smaller than full (%d)", len(sb), len(fb))
	}
	if !strings.Contains(string(sb), `data-mode="summary"`) {
		t.Error("summary output missing data-mode=summary attribute")
	}
	if !strings.Contains(string(fb), `data-mode="full"`) {
		t.Error("full output missing data-mode=full attribute")
	}
}

// TestReportCmdDefaultOutputNextToBundle verifies that running without
// --output writes into the bundle's own directory. The helper's bundle
// is named `run.fb`, which carries no timestamp to qualify the name
// with, so it takes the bare `report.html` fallback.
func TestReportCmdDefaultOutputNextToBundle(t *testing.T) {
	bundlePath := writeTestBundle(t, "0.11.0")
	rc := reportCmd([]string{bundlePath})
	if rc != 0 {
		t.Fatalf("reportCmd rc = %d", rc)
	}
	expected := filepath.Join(filepath.Dir(bundlePath), "report.html")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected report at %s: %v", expected, err)
	}
}

// TestDefaultReportPath pins the derived output name. Two bundles from
// two runs must not collide on one filename — that was the whole reason
// the fixed `report.html` default was replaced, so the distinctness
// case is asserted explicitly rather than implied by the table.
func TestDefaultReportPath(t *testing.T) {
	tests := []struct {
		name   string
		bundle string
		want   string
	}{
		{
			name:   "standard run- prefix is stripped",
			bundle: "/tmp/run-2026-07-30T10-50-37-1.fb",
			want:   "/tmp/report_2026-07-30T10-50-37-1.html",
		},
		{
			name:   "run_ underscore variant is stripped too",
			bundle: "/tmp/run_2026-07-30T10-50-37-1.fb",
			want:   "/tmp/report_2026-07-30T10-50-37-1.html",
		},
		{
			name:   "a bundle with no run prefix keeps its whole stem",
			bundle: "/tmp/nightly.fb",
			want:   "/tmp/report_nightly.html",
		},
		{
			name:   "bare run.fb has nothing to qualify with",
			bundle: "/tmp/run.fb",
			want:   "/tmp/report.html",
		},
		{
			name:   "run- with an empty stem falls back rather than report_",
			bundle: "/tmp/run-.fb",
			want:   "/tmp/report.html",
		},
		{
			name:   "output lands beside the bundle, not in the cwd",
			bundle: "/var/runs/nested/run-abc-7.fb",
			want:   "/var/runs/nested/report_abc-7.html",
		},
		{
			name:   "a bare filename resolves against the current directory",
			bundle: "run-abc-7.fb",
			want:   "report_abc-7.html",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultReportPath(tt.bundle)
			if got != filepath.FromSlash(tt.want) {
				t.Errorf("defaultReportPath(%q) = %q, want %q",
					tt.bundle, got, filepath.FromSlash(tt.want))
			}
		})
	}

	// The point of the change: distinct runs get distinct reports.
	a := defaultReportPath("/tmp/run-2026-07-30T10-50-37-1.fb")
	b := defaultReportPath("/tmp/run-2026-07-30T11-02-14-1.fb")
	if a == b {
		t.Errorf("two bundles collided on one report path: %s", a)
	}
}

// TestReportCmdMissingBundleReturnsNonZero asserts the CLI surfaces
// user errors with a non-zero exit, not a stack trace.
func TestReportCmdMissingBundleReturnsNonZero(t *testing.T) {
	rc := reportCmd([]string{filepath.Join(t.TempDir(), "nope.fb")})
	if rc == 0 {
		t.Errorf("expected non-zero rc for missing bundle, got 0")
	}
}

// TestReportCmdRejectsUnknownFlag keeps the flag parser strict so a
// typo like `--ouput` doesn't silently get treated as a bundle path.
func TestReportCmdRejectsUnknownFlag(t *testing.T) {
	rc := reportCmd([]string{"--ouput", "r.html", "bundle.fb"})
	if rc == 0 {
		t.Errorf("expected non-zero rc for unknown flag, got 0")
	}
}
