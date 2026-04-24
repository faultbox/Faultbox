// Package bundle implements the `.fb` archive format that every
// `faultbox test` run emits per RFC-025. A bundle is a single tar.gz
// file that packages trace.json, env.json, the generated replay.sh,
// and the manifest that stitches them together. Every downstream tool
// (`faultbox replay`, `faultbox inspect`, and later `faultbox report`
// in v0.11.0) consumes exactly one `.fb` file as input.
//
// The goal is reproducibility-by-default: shareable by email, check-in
// to git, or upload as a CI artifact — all on a single file.
package bundle

// SchemaVersion is the manifest.json/env.json schema version emitted by
// this package. Additive changes to either schema do NOT bump the
// version; new file kinds at the top level of the archive DO bump it.
// Tools that read a bundle MUST refuse an unknown SchemaVersion with a
// clear error rather than silently mis-parsing.
const SchemaVersion = 1

// Manifest is the top-level `manifest.json` embedded in every bundle.
// It lists every test that ran, the seed used, the Faultbox version
// that produced the bundle, and a summary. Tools read this first; the
// other artifacts in the archive are keyed off it.
type Manifest struct {
	SchemaVersion   int       `json:"schema_version"`
	FaultboxVersion string    `json:"faultbox_version"`
	RunID           string    `json:"run_id"`
	CreatedAt       string    `json:"created_at"` // RFC3339
	Seed            uint64    `json:"seed"`
	SpecRoot        string    `json:"spec_root,omitempty"`
	Tests           []TestRow `json:"tests"`
	Summary         Summary   `json:"summary"`
}

// Summary is the headline pass/fail/errored count for the run, for
// quick scanning without walking the tests array. ExpectationViolated
// is a refinement of Failed introduced by RFC-027 — rows whose expect
// predicate rejected the scenario result. Those rows are also counted
// in Failed, so existing consumers stay correct without a schema bump.
type Summary struct {
	Total               int `json:"total"`
	Passed              int `json:"passed"`
	Failed              int `json:"failed"`
	Errored             int `json:"errored"`
	ExpectationViolated int `json:"expectation_violated,omitempty"`
}

// TestRow is one row of the tests array in the manifest. Mirrors the
// existing TestTraceOutput fields that callers need without pulling in
// the larger per-test event log — the latter still lives in trace.json.
//
// RFC-027 additions (additive, does not bump SchemaVersion):
//   - Outcome gains the value "expectation_violated" for rows whose
//     expect predicate rejected the scenario result. Tools that only
//     know the v0.10.0 taxonomy can treat it as a refinement of
//     "failed" (which is why Summary.Failed still counts it).
//   - Expectation records the expect predicate's Name() (e.g.
//     "expect_success", "expect_error_within", or "lambda" for a
//     user-supplied callable) so the RFC-029 HTML report can render
//     the predicate alongside the outcome pill.
type TestRow struct {
	Name             string   `json:"name"`
	Outcome          string   `json:"outcome"` // "passed" | "failed" | "errored" | "expectation_violated"
	DurationMs       int64    `json:"duration_ms"`
	Seed             uint64   `json:"seed,omitempty"`
	FaultAssumptions []string `json:"fault_assumptions,omitempty"`
	Expectation      string   `json:"expectation,omitempty"`
}

// Env is `env.json` — the machine-readable fingerprint of the
// environment that produced the bundle. Every field is best-effort.
// Fields the runtime cannot determine are omitted (not null).
type Env struct {
	FaultboxVersion string            `json:"faultbox_version"`
	FaultboxCommit  string            `json:"faultbox_commit,omitempty"`
	HostOS          string            `json:"host_os,omitempty"`
	HostArch        string            `json:"host_arch,omitempty"`
	Kernel          string            `json:"kernel,omitempty"`
	GoToolchain     string            `json:"go_toolchain,omitempty"`
	DockerVersion   string            `json:"docker_version,omitempty"`
	RuntimeHints    []string          `json:"runtime_hints,omitempty"` // e.g. ["lima"], ["wsl"]
	Images          map[string]string `json:"images,omitempty"`        // "mysql:8.0.32" -> "sha256:..."
}
