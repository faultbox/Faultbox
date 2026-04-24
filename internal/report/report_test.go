package report

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/bundle"
)

// buildTestBundle constructs a minimal but realistic .fb archive on
// disk and returns its path. The shape mirrors what `faultbox test`
// would emit: a manifest with a small mix of pass/fail tests and a
// matrix, plus an env.json and a stubbed trace.json.
// buildTestBundle also seeds a spec/ tree so tests cover the top-level
// Spec section end-to-end.
func buildTestBundle(t *testing.T) string {
	t.Helper()
	w := bundle.NewWriter()

	manifest := bundle.Manifest{
		SchemaVersion:   bundle.SchemaVersion,
		FaultboxVersion: "0.11.0-test",
		RunID:           "2026-04-23T10-00-00-42",
		CreatedAt:       "2026-04-23T10:00:00Z",
		Seed:            42,
		SpecRoot:        "poc/mock-demo/faultbox.star",
		Tests: []bundle.TestRow{
			{Name: "test_order_creates_record", Outcome: "passed", DurationMs: 120, Seed: 42, FaultAssumptions: []string{"none"}},
			{Name: "test_inventory_survives_redis_outage", Outcome: "failed", DurationMs: 340, Seed: 42, FaultAssumptions: []string{"redis: write=EIO"}},
		},
		Summary: bundle.Summary{Total: 2, Passed: 1, Failed: 1, Errored: 0},
	}
	if err := w.AddJSON("manifest.json", manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	env := bundle.Env{
		FaultboxVersion: "0.11.0-test",
		FaultboxCommit:  "abcdef0123456789",
		HostOS:          "linux",
		HostArch:        "arm64",
		Kernel:          "6.1.0-faultbox",
		GoToolchain:     "go1.26.1",
		DockerVersion:   "25.0.2",
		RuntimeHints:    []string{"lima"},
		Images:          map[string]string{"redis:7-alpine": "sha256:deadbeefcafebabe1234"},
	}
	if err := w.AddJSON("env.json", env); err != nil {
		t.Fatalf("env: %v", err)
	}

	trace := map[string]any{
		"version":     2,
		"star_file":   "poc/mock-demo/faultbox.star",
		"duration_ms": 460,
		"pass":        1,
		"fail":        1,
		"tests": []map[string]any{
			{
				"name":        "test_order_creates_record",
				"result":      "pass",
				"seed":        42,
				"duration_ms": 120,
				"events":      []any{},
			},
			{
				"name":        "test_inventory_survives_redis_outage",
				"result":      "fail",
				"reason":      "assert_eq: expected 503, got 500",
				"failure_type": "assertion",
				"seed":        42,
				"duration_ms": 340,
				"faults": []map[string]any{
					{"service": "redis", "syscall": "write", "action": "deny", "errno": "EIO", "hits": 3},
				},
				"diagnostics": []map[string]any{
					{"level": "error", "code": "ASSERTION_MISMATCH",
						"message":    "expected 503, got 500",
						"suggestion": "check error handling"},
				},
				"events": []any{},
			},
		},
		"matrix": map[string]any{
			"scenarios": []string{"order_flow", "inventory_check"},
			"faults":    []string{"none", "redis_write_eio"},
			"cells": []map[string]any{
				{"scenario": "order_flow", "fault": "none", "passed": true, "duration_ms": 120},
				{"scenario": "order_flow", "fault": "redis_write_eio", "passed": true, "duration_ms": 140},
				{"scenario": "inventory_check", "fault": "none", "passed": true, "duration_ms": 130},
				{"scenario": "inventory_check", "fault": "redis_write_eio", "passed": false,
					"duration_ms": 340, "reason": "assert_eq failed"},
			},
			"total":  4,
			"passed": 3,
			"failed": 1,
		},
	}
	if err := w.AddJSON("trace.json", trace); err != nil {
		t.Fatalf("trace: %v", err)
	}
	w.AddFile("replay.sh", []byte("#!/bin/sh\nfaultbox replay run-2026-04-23-42.fb\n"))
	w.AddFile("spec/faultbox.star", []byte(
		"orders = service(\"orders\", \"bin/orders\")\n"+
			"redis = mock_service(\"redis\", \"@faultbox/mocks/redis.star\")\n"+
			"\n"+
			"def test_order_creates_record():\n"+
			"    orders.post(\"/orders\", {\"id\": 1})\n"+
			"\n"+
			"def test_inventory_survives_redis_outage():\n"+
			"    fault(redis, write=\"EIO\")\n"+
			"    assert_eq(orders.post(\"/orders\", {}).status, 503)\n"))

	dst := filepath.Join(t.TempDir(), "run-2026-04-23-42.fb")
	if err := w.WriteTo(dst); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return dst
}

func TestBuildEmitsSelfContainedHTML(t *testing.T) {
	path := buildTestBundle(t)
	r, err := bundle.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}

	var buf bytes.Buffer
	if err := Build(&buf, r, "0.11.0-test"); err != nil {
		t.Fatalf("build report: %v", err)
	}

	got := buf.String()

	// Structural markers: HTML shell, inlined script tag, CSS block.
	checks := []string{
		"<!DOCTYPE html>",
		`id="faultbox-data"`,
		`type="application/json"`,
		"<style>",
		":root {",
		"faultbox",
		// Manifest data should be visible in the inlined JSON.
		"test_inventory_survives_redis_outage",
		"2026-04-23T10-00-00-42",
		// Matrix payload inlined.
		"redis_write_eio",
		// Env.
		"go1.26.1",
		// Generator line in footer.
		"0.11.0-test",
	}
	for _, c := range checks {
		if !strings.Contains(got, c) {
			t.Errorf("report missing %q", c)
		}
	}

	// The inlined data block should not contain a literal `</` sequence
	// (it should have been rewritten to `<\/`). The broader "no `</`
	// inside any script body" invariant is covered by
	// TestEmbeddedScriptsStayOpen; this narrower check is kept because
	// the data-block route is the one most likely to carry user content.
	dataStart := strings.Index(got, `id="faultbox-data"`)
	dataEnd := strings.Index(got[dataStart:], "</script>")
	if dataStart < 0 || dataEnd < 0 {
		t.Fatal("could not locate data script block")
	}
	dataBlock := got[dataStart : dataStart+dataEnd]
	if strings.Contains(dataBlock, "</") {
		t.Errorf("data block contains raw </; inlineJSON did not escape")
	}
}

func TestBuildMatrixEncodedInPayload(t *testing.T) {
	path := buildTestBundle(t)
	r, err := bundle.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var buf bytes.Buffer
	if err := Build(&buf, r, "0.11.0-test"); err != nil {
		t.Fatalf("build: %v", err)
	}
	// Extract the JSON payload from between the two script markers and
	// ensure it round-trips to something with a matrix and at least one
	// failed cell.
	html := buf.String()
	tagStart := strings.Index(html, `<script id="faultbox-data" type="application/json">`)
	if tagStart < 0 {
		t.Fatal("no data tag")
	}
	payloadStart := tagStart + len(`<script id="faultbox-data" type="application/json">`)
	payloadEnd := strings.Index(html[payloadStart:], "</script>")
	if payloadEnd < 0 {
		t.Fatal("unterminated data tag")
	}
	payload := html[payloadStart : payloadStart+payloadEnd]
	// Undo the inline-JSON defence so strict parsers accept it.
	payload = strings.ReplaceAll(payload, `<\/`, "</")

	var decoded struct {
		Manifest struct {
			Summary struct {
				Failed int `json:"failed"`
			} `json:"summary"`
		} `json:"manifest"`
		Trace struct {
			Matrix struct {
				Cells []struct {
					Passed bool `json:"passed"`
				} `json:"cells"`
			} `json:"matrix"`
		} `json:"trace"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("parse inlined payload: %v", err)
	}
	if decoded.Manifest.Summary.Failed != 1 {
		t.Errorf("expected 1 failed test, got %d", decoded.Manifest.Summary.Failed)
	}
	if len(decoded.Trace.Matrix.Cells) != 4 {
		t.Errorf("expected 4 matrix cells, got %d", len(decoded.Trace.Matrix.Cells))
	}
	failed := 0
	for _, c := range decoded.Trace.Matrix.Cells {
		if !c.Passed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("expected 1 failed matrix cell, got %d", failed)
	}
}

func TestInlineJSONEscapesScriptTerminator(t *testing.T) {
	got := inlineJSON([]byte(`{"a":"</script><script>alert(1)</script>"}`))
	if strings.Contains(got, "</") {
		t.Errorf("inlineJSON left raw </: %s", got)
	}
	if !strings.Contains(got, `<\/script>`) {
		t.Errorf("inlineJSON did not rewrite to <\\/script>: %s", got)
	}
}

// TestEmbeddedScriptsStayOpen is a regression guard against the exact
// bug the v0.11.0 Phase 1 demo surfaced: if the embedded app.js source
// contains a literal "</script>" sequence (typically inside a doc
// comment), the HTML parser closes the outer script tag early and the
// rest of the JS renders as visible text. The fix is to run every
// script body through escapeScriptContent; this test proves the final
// HTML has no stray "</" inside any script tag.
func TestEmbeddedScriptsStayOpen(t *testing.T) {
	path := buildTestBundle(t)
	r, err := bundle.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var buf bytes.Buffer
	if err := Build(&buf, r, "0.11.0-test"); err != nil {
		t.Fatalf("build: %v", err)
	}
	html := buf.String()

	// Walk every <script> ... </script> range and confirm no raw "</"
	// appears before the closing marker we find last.
	low := strings.ToLower(html)
	for off := 0; ; {
		open := strings.Index(low[off:], "<script")
		if open < 0 {
			break
		}
		open += off
		// Skip past the `>` that closes the opening tag.
		bodyStart := strings.Index(low[open:], ">")
		if bodyStart < 0 {
			t.Fatalf("unterminated <script open at %d", open)
		}
		bodyStart += open + 1
		bodyEnd := strings.Index(low[bodyStart:], "</script>")
		if bodyEnd < 0 {
			t.Fatalf("unterminated script body starting at %d", bodyStart)
		}
		bodyEnd += bodyStart
		body := html[bodyStart:bodyEnd]
		if strings.Contains(body, "</") {
			idx := strings.Index(body, "</")
			t.Errorf("unescaped </ inside <script> body (char %d): %q",
				idx, body[idx:min(idx+40, len(body))])
		}
		off = bodyEnd + len("</script>")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAppJSHasNoRegexWithScriptCloseFragment guards against a subtle
// variant of the </script escape bug: if the embedded app.js contains
// a regular-expression literal whose body includes `</` (e.g.
// /</g), escapeScriptContent rewrites it to `<\/`, which — unlike in
// a string literal — is a semantically different regex (or in the
// worst case, one that spans to the next `/` elsewhere on the line,
// producing a SyntaxError at page load). The fix is to write such
// regexes with a character class (/[<]/g) so the raw two-byte
// sequence `</` never appears in the source. This test asserts that
// invariant on the embedded JS directly so future edits can't
// silently reintroduce it.
func TestAppJSHasNoRegexWithScriptCloseFragment(t *testing.T) {
	// We want to catch regex literals whose body contains `</`. Scan
	// every line for the pattern `/<`... and flag any occurrence that
	// isn't inside a string literal. A simple heuristic is enough:
	// flag any `/</` substring that starts within a line (regex
	// bodies don't cross lines in our source). False positives from
	// comments (`//<`) are excluded by skipping `//`-prefixed lines.
	for i, line := range strings.Split(appJS, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(line, "/</") {
			t.Errorf("line %d has a /</ regex fragment that will be mangled "+
				"by escapeScriptContent; rewrite with /[<]/ character class:\n  %s",
				i+1, line)
		}
	}
}
