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
// --output writes `report.html` in the bundle's directory (the muscle-
// memory path users rely on when iterating).
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
