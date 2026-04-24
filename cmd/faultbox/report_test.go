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

	// Shell markers and the inlined data payload.
	wants := []string{
		"<!DOCTYPE html>",
		`id="faultbox-data"`,
		":root {",             // CSS
		"window.__FAULTBOX__", // app.js writes this after parse
		"testrun",             // run ID from the synthetic bundle
		"test_ok",
		"test_bad",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("report missing %q", w)
		}
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
