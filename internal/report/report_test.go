package report

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/bundle"
)

// extractDataPayload pulls the gzip+base64 block out of a rendered
// report and returns the decompressed JSON bytes. Tests use this to
// assert on report contents after RFC-031 Phase 1 swapped the inline
// JSON tag for a compressed binary one.
func extractDataPayload(t *testing.T, html string) []byte {
	t.Helper()
	const openMarker = `id="faultbox-data-gz"`
	i := strings.Index(html, openMarker)
	if i < 0 {
		t.Fatalf("no %s tag in report", openMarker)
	}
	// Scan forward to the end of the opening <script ...> tag.
	gt := strings.Index(html[i:], ">")
	if gt < 0 {
		t.Fatal("unterminated <script> opening tag")
	}
	bodyStart := i + gt + 1
	bodyEnd := strings.Index(html[bodyStart:], "</script>")
	if bodyEnd < 0 {
		t.Fatal("unterminated <script> body")
	}
	b64 := strings.TrimSpace(html[bodyStart : bodyStart+bodyEnd])
	gz, err := base64.URLEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gzr.Close()
	raw, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return raw
}

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

	// Structural markers of the outer HTML shell — these are literal
	// strings that must appear in the rendered report regardless of
	// how the data payload is encoded.
	shellChecks := []string{
		"<!DOCTYPE html>",
		`id="faultbox-data-gz"`,
		`type="application/octet-stream"`,
		`data-encoding="gzip+base64"`,
		"<style>",
		":root {",
		"faultbox",
		// Generator line in footer (not inside the compressed data).
		"0.11.0-test",
	}
	for _, c := range shellChecks {
		if !strings.Contains(got, c) {
			t.Errorf("report missing %q", c)
		}
	}

	// The bundle contents are only visible after gzip+base64 decode.
	// Round-trip the payload and assert the strings that used to live
	// in the plain-JSON inline block still appear.
	raw := extractDataPayload(t, got)
	contentChecks := []string{
		"test_inventory_survives_redis_outage",
		"2026-04-23T10-00-00-42",
		"redis_write_eio",
		"go1.26.1",
	}
	for _, c := range contentChecks {
		if !bytes.Contains(raw, []byte(c)) {
			t.Errorf("decompressed payload missing %q", c)
		}
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
	raw := extractDataPayload(t, buf.String())

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
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("parse decompressed payload: %v", err)
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

// TestBuildSummaryModeDropsTrace is the primary contract for
// --summary mode (RFC-031 Phase 1): the resulting report's inlined
// payload carries manifest + env but no trace, making the file
// dramatically smaller than the full-mode equivalent built from the
// same bundle.
func TestBuildSummaryModeDropsTrace(t *testing.T) {
	path := buildTestBundle(t)
	r, err := bundle.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	var full, summary bytes.Buffer
	if err := BuildWithOptions(&full, r, "0.12-test", Options{}); err != nil {
		t.Fatalf("full build: %v", err)
	}
	r2, _ := bundle.Open(path)
	if err := BuildWithOptions(&summary, r2, "0.12-test", Options{Summary: true}); err != nil {
		t.Fatalf("summary build: %v", err)
	}

	fullPayload := extractDataPayload(t, full.String())
	summaryPayload := extractDataPayload(t, summary.String())

	// The full payload must carry the trace (matrix.cells live there)
	// and summary must not.
	if !bytes.Contains(fullPayload, []byte(`"trace"`)) {
		t.Error("full payload missing trace field")
	}
	if bytes.Contains(summaryPayload, []byte(`"trace"`)) {
		t.Error("summary payload still contains trace field")
	}
	// Manifest must survive in summary mode — it's the spine of the
	// matrix + tests table + coverage sections.
	if !bytes.Contains(summaryPayload, []byte("test_inventory_survives_redis_outage")) {
		t.Error("summary payload missing manifest test data")
	}

	// Summary HTML must be materially smaller than full. With a test
	// bundle this trimmed, we demand only that summary < full; real
	// bundles show an order-of-magnitude difference.
	if summary.Len() >= full.Len() {
		t.Errorf("summary (%d) not smaller than full (%d)", summary.Len(), full.Len())
	}

	// The header size banner should show the mode so users opening
	// the report can tell at a glance.
	if !strings.Contains(summary.String(), `data-mode="summary"`) {
		t.Error("summary report missing data-mode=summary on data tag")
	}
	if !strings.Contains(full.String(), `data-mode="full"`) {
		t.Error("full report missing data-mode=full on data tag")
	}
}

// TestGzipBytesShrinksRedundantPayload asserts the headline RFC-031
// promise on a payload that resembles real event data (highly
// redundant JSON). On a 100 KB highly-repetitive blob, the encoded
// (gzip + URL-safe base64) output must be at least 3x smaller than
// the original — guarding against a future change that disables
// compression or picks a degenerate level.
func TestGzipBytesShrinksRedundantPayload(t *testing.T) {
	// Simulate an event log: 1000 copies of the same JSON object.
	// Real bundles look similar (per-syscall events with repeating
	// field names and small value variation).
	var b bytes.Buffer
	b.WriteString("[")
	for i := 0; i < 1000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"syscall","name":"write","fd":3,"bytes":4096,"errno":0,"ts_ns":12345}`)
	}
	b.WriteString("]")
	raw := b.Bytes()

	gz, err := gzipBytes(raw)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	encoded := base64.URLEncoding.EncodeToString(gz)

	ratio := float64(len(raw)) / float64(len(encoded))
	if ratio < 3.0 {
		t.Errorf("compression ratio = %.2fx, want >= 3.0x (raw=%d encoded=%d)",
			ratio, len(raw), len(encoded))
	}
}

// TestBuildGzipShrinksPayload pins the headline benefit of
// Phase 1: on a realistic payload (padded to expose gzip's redundancy
// win), the base64'd gzip output must be meaningfully smaller than
// the raw JSON would have been. Guards against future refactors that
// accidentally disable compression.
func TestBuildGzipShrinksPayload(t *testing.T) {
	path := buildTestBundle(t)
	r, err := bundle.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Raw size: what the old v0.11 path would have inlined.
	data, err := gatherData(r)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	rawJSON, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	if err := Build(&buf, r, "0.12-test"); err != nil {
		t.Fatalf("build: %v", err)
	}
	encoded := extractEncodedSize(t, buf.String())

	// The test bundle is tiny, so the absolute shrink is small, but
	// the compressed+base64 size must never exceed the raw size. This
	// is the guardrail; on production-sized bundles the ratio is
	// 4-6x in practice.
	if encoded >= len(rawJSON) {
		t.Errorf("gzip+base64 not shrinking: raw=%d encoded=%d", len(rawJSON), encoded)
	}
}

// extractEncodedSize returns the length of the base64 payload inside
// the faultbox-data-gz script tag, i.e. what was inlined into the
// HTML (before gzip decompression).
func extractEncodedSize(t *testing.T, html string) int {
	t.Helper()
	i := strings.Index(html, `id="faultbox-data-gz"`)
	if i < 0 {
		t.Fatal("no data tag")
	}
	gt := strings.Index(html[i:], ">")
	bodyStart := i + gt + 1
	bodyEnd := strings.Index(html[bodyStart:], "</script>")
	return len(strings.TrimSpace(html[bodyStart : bodyStart+bodyEnd]))
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
