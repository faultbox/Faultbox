package bundle

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

// BuildInput is the everything-in-one-place argument to Build. Callers
// (cmd/faultbox) fill this from the CLI + suite result and hand it
// off. Keeping the surface as a struct means new optional pieces
// (spec/ directory, services/ logs, …) can land in future phases
// without re-threading every call site.
type BuildInput struct {
	// FaultboxVersion / FaultboxCommit get stamped into manifest.json
	// and env.json. Commit is best-effort; empty is allowed.
	FaultboxVersion string
	FaultboxCommit  string

	// Seed is whatever the runner used. Recorded in manifest so
	// `faultbox replay` can re-run deterministically.
	Seed uint64

	// SpecRoot is the path to the top-level .star file. Stamped into
	// manifest for provenance; we don't (yet) copy the file into the
	// bundle — that's Phase 4 of RFC-025.
	SpecRoot string

	// Tests carries the summary per test (name, outcome, duration).
	// Caller builds this from SuiteResult; bundle package stays free
	// of the `star` import to avoid a cycle.
	Tests []TestRow

	// CreatedAt defaults to time.Now() if zero.
	CreatedAt time.Time

	// Trace is the bytes of the existing --output trace.json
	// representation. Passed in pre-marshalled so Build doesn't need
	// to import the star package.
	Trace []byte

	// Specs maps bundle-relative path → file contents for every
	// .star file the Starlark runtime touched (root + transitive
	// local load()s). `@faultbox/...` stdlib modules are excluded —
	// the consuming binary already ships them. Stored under `spec/`
	// in the archive so `faultbox replay` can rehydrate the exact
	// source tree. RFC-025 Phase 4.
	Specs map[string][]byte

	// Crash is non-nil when the suite was terminated by a Go runtime
	// panic. Issue #76: emit a partial bundle marked as such instead
	// of losing every test result to the dying process.
	Crash *CrashInfo
}

// Build assembles a Writer populated with manifest.json, env.json,
// trace.json, and replay.sh. The caller writes it to disk by calling
// WriteTo on the returned writer. Keeping the "write" step separate
// means tests can assert on in-memory bundles without touching the
// filesystem.
func Build(in BuildInput) (*Writer, string, error) {
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}

	manifest := Manifest{
		SchemaVersion:   SchemaVersion,
		FaultboxVersion: in.FaultboxVersion,
		RunID:           NewRunID(),
		CreatedAt:       in.CreatedAt.UTC().Format(time.RFC3339),
		Seed:            in.Seed,
		SpecRoot:        in.SpecRoot,
		Tests:           in.Tests,
		Summary:         summaryFromTests(in.Tests),
		Crash:           in.Crash,
	}
	env := GatherEnv(in.FaultboxVersion, in.FaultboxCommit)

	filename := DefaultFilename(in.CreatedAt, in.Seed)

	w := NewWriter()
	if err := w.AddJSON("manifest.json", manifest); err != nil {
		return nil, "", fmt.Errorf("manifest: %w", err)
	}
	if err := w.AddJSON("env.json", env); err != nil {
		return nil, "", fmt.Errorf("env: %w", err)
	}
	if len(in.Trace) > 0 {
		w.AddFile("trace.json", ensureJSONTrailingNewline(in.Trace))
	}
	w.AddFile("replay.sh", GenerateReplayScript(filename, in.FaultboxVersion, in.CreatedAt))

	// RFC-025 Phase 4: archive every local .star file under spec/.
	// Empty map is a no-op — bundles from callers that skip this
	// field (tests, legacy code paths) still write a valid archive.
	for rel, data := range in.Specs {
		w.AddFile("spec/"+rel, data)
	}

	return w, filename, nil
}

// summaryFromTests tallies the pass/fail/error counts from a slice of
// rows. RFC-027's "expectation_violated" is a refinement of "failed"
// (bump both Failed and ExpectationViolated so legacy consumers that
// only know the v0.10.0 taxonomy still see the row in the failed
// bucket). Issue #75's "fault_bypassed" is a refinement of "passed"
// (bump both Passed and FaultBypassed so CI pipelines gating on
// Failed don't flap but users can still see the ambiguity signal).
func summaryFromTests(rows []TestRow) Summary {
	var s Summary
	for _, r := range rows {
		s.Total++
		switch r.Outcome {
		case "passed":
			s.Passed++
		case "failed":
			s.Failed++
		case "errored":
			s.Errored++
		case "expectation_violated":
			s.Failed++
			s.ExpectationViolated++
		case "fault_bypassed":
			s.Passed++
			s.FaultBypassed++
		}
	}
	return s
}

// ensureJSONTrailingNewline makes sure trace.json ends with a newline,
// which keeps `jq`/`cat` output from looking awkward when users pull
// the file out of the archive. Not a correctness concern for the JSON
// itself — Go decoders don't care — but a small UX nicety.
func ensureJSONTrailingNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	out := make([]byte, len(b)+1)
	copy(out, b)
	out[len(b)] = '\n'
	return out
}

// MarshalTraceForBundle is a small adapter: callers that already have
// a json.Marshal-able trace object (like star.TraceOutput) can use
// this instead of doing the marshal themselves. Separate from Build
// so the bundle package stays import-clean of the star package.
func MarshalTraceForBundle(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal trace: %w", err)
	}
	return data, nil
}

// ResolvePath computes the final bundle filesystem path given the
// user's --bundle=<path> override, the $FAULTBOX_BUNDLE_DIR env var,
// and the default filename. Order of precedence: explicit override >
// env dir > cwd.
func ResolvePath(explicitPath, envDir, defaultFilename string) string {
	if explicitPath != "" {
		return explicitPath
	}
	if envDir != "" {
		return filepath.Join(envDir, defaultFilename)
	}
	return defaultFilename
}
