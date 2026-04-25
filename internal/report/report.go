// Package report builds a single self-contained `report.html` from one
// `.fb` bundle (RFC-025). It is the implementation behind `faultbox
// report <bundle.fb>` shipped in v0.11.0 per RFC-029.
//
// The output is one HTML file. No network fetches, no external assets.
// Users can email it, attach it in Slack, commit it to git, or publish
// it as a CI artifact — and it keeps working, offline, forever. The
// same "one file, inlined data" trick R-Studio notebooks pioneered for
// data science.
//
// Data flow:
//
//	bundle (tar.gz) ──► Reader ──► Data (manifest + env + trace) ──►
//	  html/template ──► report.html (CSS+JS embedded via go:embed)
//
// The Go side is a dumb renderer: it gathers the three JSONs from the
// bundle, builds a single JS object literal on window.__FAULTBOX__, and
// writes the HTML shell. All visualisation logic lives in app.js; all
// styling lives in style.css. Both are //go:embed'd at compile time so
// the binary remains a single artifact.
package report

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"strings"
	"time"

	"github.com/faultbox/Faultbox/internal/bundle"
)

//go:embed template.html
var templateHTML string

//go:embed app.js
var appJS string

//go:embed style.css
var styleCSS string

// Data is the bundle contents shaped for the HTML template. Only the
// fields listed here are serialised into the inlined
// `window.__FAULTBOX__` object; everything the JS side needs must go
// through here.
//
// `Specs` carries every `spec/*.star` file captured in the bundle
// (RFC-025 Phase 4). The JS renders a top-level collapsible "Spec"
// section and cross-links from the coverage table + drill-down to
// specific lines in these files. Keys are paths relative to the
// archive root, e.g. "spec/faultbox.star".
type Data struct {
	Manifest *bundle.Manifest  `json:"manifest"`
	Env      *bundle.Env       `json:"env,omitempty"`
	Trace    json.RawMessage   `json:"trace,omitempty"`
	Specs    map[string]string `json:"specs,omitempty"`
}

// templateContext carries the values the html/template substitutes.
// Fields are either safe HTML (CSS/JS we control) or an
// already-safe-for-<script> base64 string.
//
// Data is inlined as gzip+base64 per RFC-031 (#83). Typical shrink
// on a PoC-scale bundle: ~22 MB raw events JSON → ~4-5 MB base64.
// The JS side decompresses on open via the DecompressionStream API
// (Chrome 80+, Safari 16.4+, Firefox 113+). Base64 is chosen over
// hex because it yields ~33% rather than 100% overhead and contains
// no characters that the HTML parser treats specially inside a
// <script type="application/octet-stream"> block.
type templateContext struct {
	Title           string
	Subtitle        string
	GeneratedAt     string
	FaultboxVersion string
	StyleCSS        template.CSS
	AppJS           template.JS
	DataB64         template.JS
	SizeBanner      string
	Mode            string
}

// Options tunes report rendering. Added in v0.12 (RFC-031 #83) to
// give `faultbox report` a smaller default output and an opt-in
// summary mode for CI artifact uploads.
//
//   - Summary=true drops the bundle's trace entirely, leaving only
//     the manifest and env data the matrix/tests/coverage sections
//     need. Typical output: <100 KB. The drill-down modal becomes a
//     "re-run to see details" hint.
//
//   - FullEvents=true disables Phase 3 downsampling — the JSON
//     payload carries every event from the bundle. Default behaviour
//     keeps faults/violations/lifecycle anchors plus the first and
//     last EventsHeadTail rows per test plus a ±EventsAnchorWindow
//     window around each anchor; everything else is dropped. The
//     trace.json blob in the bundle is never modified.
type Options struct {
	Summary    bool
	FullEvents bool
}

// Downsampling defaults — Phase 3 (RFC-031). Tuned so the typical
// PoC-scale run keeps every fault/violation event plus enough
// surrounding context for forensic value, while shedding the
// repetitive read/write tail that dominates real syscall logs.
const (
	eventsHeadTail     = 50 // first N + last N events per test always kept
	eventsAnchorWindow = 25 // ±N events around each anchor always kept
)

// anchorTypes are event types whose context must always survive
// downsampling: faults, lifecycle, scenario steps, and anything the
// runtime flagged as an invariant violation. Syscalls are
// deliberately absent — they're the bulk we shed.
var anchorTypes = map[string]bool{
	"fault_applied":          true,
	"fault_removed":          true,
	"fault_skipped_no_seccomp": true,
	"fault_zero_traffic":     true,
	"violation":              true,
	"service_started":        true,
	"service_ready":          true,
	"service_stopped":        true,
	"step_send":              true,
	"step_recv":              true,
}

// Build reads a bundle and writes a single self-contained report.html
// to the writer. All styles, scripts, and bundle data are inlined; the
// resulting file has zero external dependencies and can be emailed,
// Slack'd, or checked into git without breaking.
//
// Data is inlined as gzip+base64 (RFC-031) and decompressed by the
// page on open via the DecompressionStream browser API.
//
// faultboxVersion is the version of the caller — used for the
// generator footer and for a best-effort "generated by" line; it does
// not gate rendering on version compatibility (RFC-025 inspect policy
// applies here too: never refuse on a read-only path).
func Build(w io.Writer, r *bundle.Reader, faultboxVersion string) error {
	return BuildWithOptions(w, r, faultboxVersion, Options{})
}

// BuildWithOptions is Build with explicit rendering options.
func BuildWithOptions(w io.Writer, r *bundle.Reader, faultboxVersion string, opts Options) error {
	data, err := gatherData(r)
	if err != nil {
		return fmt.Errorf("gather bundle data: %w", err)
	}

	if opts.Summary {
		// Drop the trace — by far the biggest contributor to
		// report size on PoC-sized bundles. Downstream sections that
		// need it (matrix drill-down, swim-lane viewer) detect the
		// absence and render a "re-run with full trace" hint.
		data.Trace = nil
	} else if !opts.FullEvents && len(data.Trace) > 0 {
		// Phase 3 downsampling — keep only the events that carry
		// forensic value (anchors + head/tail + ±window). Power
		// users who want the full firehose pass --full-events.
		downsampled, _, _, derr := downsampleTrace(data.Trace)
		if derr == nil {
			data.Trace = downsampled
		}
		// On error: fall through with the original trace. Worse
		// to fail rendering a real report than to ship a bigger one.
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal report payload: %w", err)
	}

	gz, err := gzipBytes(payload)
	if err != nil {
		return fmt.Errorf("gzip payload: %w", err)
	}
	// URL-safe base64 alphabet (- and _ instead of + and /). Avoids
	// html/template HTML-escaping `+` to `&#43;` inside <script> tags
	// with unrecognised type= attributes. The JS side reverses the
	// substitution before calling atob().
	b64 := base64.URLEncoding.EncodeToString(gz)

	ctx := templateContext{
		Title:           reportTitle(data.Manifest),
		Subtitle:        reportSubtitle(data.Manifest),
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		FaultboxVersion: faultboxVersion,
		StyleCSS:        template.CSS(styleCSS),
		// The app JS still sits inside a <script> tag; a literal
		// "</script" in its body would close the tag early and fold
		// the remainder into HTML. The data block is now base64
		// (no "<" or "/" in the alphabet) so it needs no escape.
		AppJS:      template.JS(escapeScriptContent(appJS)),
		DataB64:    template.JS(b64),
		SizeBanner: sizeBanner(len(payload), len(b64), opts.Summary),
		Mode:       modeLabel(opts.Summary),
	}

	t, err := template.New("report").Parse(templateHTML)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// BuildToFile is Build's path-based sibling. It writes atomically
// (tmp + rename) so a half-written report never appears on disk —
// important in CI where the report might be picked up by a watcher
// the moment it lands.
func BuildToFile(path string, r *bundle.Reader, faultboxVersion string) error {
	return BuildToFileWithOptions(path, r, faultboxVersion, Options{})
}

// BuildToFileWithOptions is BuildToFile with explicit options.
func BuildToFileWithOptions(path string, r *bundle.Reader, faultboxVersion string, opts Options) error {
	tmp, err := os.CreateTemp(dirOf(path), ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := BuildWithOptions(tmp, r, faultboxVersion, opts); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// downsampleTrace walks a trace.json blob, downsamples each test's
// event stream per the Phase 3 policy, and returns a modified blob
// alongside the kept/dropped totals. Tests that have no events or
// fewer events than (2*headTail) are left untouched — there's
// nothing to gain from sampling a stream that's already small.
//
// The trace's other fields (matrix, version, summary) pass through
// unchanged.
func downsampleTrace(raw json.RawMessage) (json.RawMessage, int, int, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw, 0, 0, err
	}
	tests, ok := top["tests"].([]any)
	if !ok {
		return raw, 0, 0, nil
	}
	var totalKept, totalDropped int
	for i, ti := range tests {
		m, _ := ti.(map[string]any)
		if m == nil {
			continue
		}
		events, _ := m["events"].([]any)
		original := len(events)
		if original <= 2*eventsHeadTail {
			totalKept += original
			continue
		}
		kept := downsampleEvents(events)
		dropped := original - len(kept)
		m["events"] = kept
		m["events_total"] = original
		m["events_dropped"] = dropped
		totalKept += len(kept)
		totalDropped += dropped
		tests[i] = m
	}
	if totalDropped > 0 {
		top["events_downsampled"] = true
	}
	out, err := json.Marshal(top)
	if err != nil {
		return raw, 0, 0, err
	}
	return out, totalKept, totalDropped, nil
}

// downsampleEvents picks the subset to keep: anchors + head + tail
// + ±anchorWindow around each anchor. Indexes are first computed as
// a "keep" set, then the events are extracted in original order so
// that the resulting slice still satisfies the "events are sorted
// by seq" invariant the swim-lane renderer relies on.
func downsampleEvents(events []any) []any {
	n := len(events)
	keep := make([]bool, n)

	// Head + tail.
	for i := 0; i < eventsHeadTail && i < n; i++ {
		keep[i] = true
	}
	for i := n - eventsHeadTail; i < n; i++ {
		if i >= 0 {
			keep[i] = true
		}
	}

	// Anchors + ±window.
	for i := 0; i < n; i++ {
		ev, _ := events[i].(map[string]any)
		if ev == nil {
			continue
		}
		t, _ := ev["type"].(string)
		if !anchorTypes[t] {
			continue
		}
		lo := i - eventsAnchorWindow
		hi := i + eventsAnchorWindow
		if lo < 0 {
			lo = 0
		}
		if hi >= n {
			hi = n - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}

	out := make([]any, 0, n)
	for i, k := range keep {
		if k {
			out = append(out, events[i])
		}
	}
	return out
}

// gzipBytes compresses b with gzip at default compression. Default
// level is the right choice for report data — the payload is
// structured JSON with high redundancy, so the incremental shrink
// from BestCompression over DefaultCompression is small while the
// CPU cost is ~3x.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sizeBanner renders the header indicator that tells the reader
// whether this report is full or summary and how big the payload is.
// Users cared about size after hitting upload limits with 23 MB
// reports (customer feedback, v0.11.1 regression report).
func sizeBanner(rawBytes, encodedBytes int, summary bool) string {
	kind := "full"
	if summary {
		kind = "summary"
	}
	return fmt.Sprintf("%s · %s → %s (gzip+base64)",
		kind, humanBytes(rawBytes), humanBytes(encodedBytes))
}

func modeLabel(summary bool) string {
	if summary {
		return "summary"
	}
	return "full"
}

// humanBytes formats a byte count in IEC-ish units. Reports never
// exceed GB in practice so we stop at MB for readability; a TB-sized
// report would be a bug long before it was a display issue.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

func gatherData(r *bundle.Reader) (*Data, error) {
	m := r.Manifest()
	if m == nil {
		return nil, fmt.Errorf("bundle has no manifest.json")
	}

	d := &Data{Manifest: m, Env: r.Env()}

	// trace.json may be absent on older bundles or for pass-only
	// runs where no trace was emitted; the report still renders
	// without it (just no drill-down or matrix detail).
	if raw, err := r.File("trace.json"); err == nil && len(raw) > 0 {
		d.Trace = json.RawMessage(raw)
	}

	// Collect every spec/*.star file so the JS side can render the
	// top-level Spec section and cross-link from coverage/topology
	// to specific service/test definitions.
	specs := map[string]string{}
	for _, name := range r.Files() {
		if !strings.HasPrefix(name, "spec/") || !strings.HasSuffix(name, ".star") {
			continue
		}
		raw, err := r.File(name)
		if err != nil || len(raw) == 0 {
			continue
		}
		specs[name] = string(raw)
	}
	if len(specs) > 0 {
		d.Specs = specs
	}
	return d, nil
}

// escapeScriptContent neutralises the one sequence that would close a
// surrounding <script> tag if it appeared in the embedded JS source —
// "</" anywhere inside (most commonly "</script>" in a comment). A
// backslash escape is harmless in both string literals and block
// comments, so we apply it unconditionally rather than trying to be
// clever about where in the source the sequence appears.
func escapeScriptContent(s string) string {
	return strings.ReplaceAll(s, "</", "<\\/")
}

func reportTitle(m *bundle.Manifest) string {
	if m == nil || m.RunID == "" {
		return "Faultbox Report"
	}
	return fmt.Sprintf("Faultbox Report — %s", m.RunID)
}

func reportSubtitle(m *bundle.Manifest) string {
	if m == nil {
		return ""
	}
	s := m.Summary
	switch {
	case s.Total == 0:
		return "No tests ran."
	case s.Failed == 0 && s.Errored == 0:
		return fmt.Sprintf("All %d check%s held up under the injected faults.",
			s.Total, plural(s.Total))
	case s.Errored > 0:
		return fmt.Sprintf("%d of %d checks held; %d regressed, %d errored.",
			s.Passed, s.Total, s.Failed, s.Errored)
	default:
		return fmt.Sprintf("%d of %d checks held; %d regressed under fault.",
			s.Passed, s.Total, s.Failed)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
