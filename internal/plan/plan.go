// Package plan represents the deterministic plan tree for a loaded
// Faultbox spec — every test instance the spec will produce, with the
// composition axes (fault_matrix, fault_scenario, etc.) made explicit.
//
// The plan tree is RFC-042's foundational data structure: `faultbox
// plan` renders it, `faultbox test` serializes it into the bundle as
// `plan.json`, and the report's Plan tab reads it back. v0.13.0-rc1
// ships analysis-only: enumeration, text/JSON rendering, bundle write.
// Spec-level interleaving execution and probability fan-out are RFC
// §8.8/§8.9 and land in rc2.
//
// The package depends on internal/star purely to read the loaded
// runtime — no Starlark is executed during enumeration, and no service
// is launched. This is what makes `faultbox plan` cheap (<100ms even
// for large specs) and L0-deterministic (RFC-040): the tree is a pure
// function of the loaded runtime state.
package plan

// SchemaVersion is the version of the plan-tree JSON schema. Bumped
// when a backwards-incompatible field change ships; older readers in
// `faultbox replay` consult this before reading optional fields.
const SchemaVersion = 1

// PlanTree is the deterministic enumeration of every test instance a
// loaded spec will produce. Returned by Enumerate.
type PlanTree struct {
	SchemaVersion int             `json:"schema_version"`
	SpecPath      string          `json:"spec_path,omitempty"`
	Seed          *uint64         `json:"seed,omitempty"`
	Determinism   PlanDeterminism `json:"determinism"`
	Tests         []PlanTest      `json:"tests"`
	Totals        PlanTotals      `json:"totals"`
	// Topology is populated by Enumerate. Coverage attaches via
	// WithCoverage when the caller wants edge-coverage data — the
	// CLI's --coverage flag and `faultbox test`'s bundle path both
	// run it. Plans without coverage have Coverage == nil; the JSON
	// shape stays stable across both.
	Topology PlanTopology `json:"topology"`
	// Coverage is populated by WithCoverage (RFC-042 §5.3, §8.4). Nil
	// when the caller skipped coverage analysis — Enumerate alone
	// doesn't compute it, so `faultbox plan` without --coverage and
	// `faultbox test` (which always includes coverage in plan.json
	// when enabled) split cleanly.
	Coverage *PlanCoverage `json:"coverage,omitempty"`
}

// PlanDeterminism mirrors RFC-040's contract for the run. Plan output
// surfaces this so users see at a glance whether the tree they're
// looking at was authored under L0 (plan-determined) or L1 (event-log
// determined).
type PlanDeterminism struct {
	Level    string `json:"level"`              // "L0" | "L1" (other levels rejected at spec load today)
	Runtime  string `json:"runtime"`            // "default" today; "gvisor" reserved
	Strict   bool   `json:"strict"`             // RFC-040 §8.3 strict-determinism
	Explicit bool   `json:"explicit,omitempty"` // true iff spec called determinism()
}

// PlanTest is one top-level test entry in the plan tree. Derived from
// `def test_*()`, `scenario()`, `fault_scenario()`, `fault_matrix()`,
// or `test()`. The Kind discriminator records which builtin produced
// this entry; tooling that filters by category uses it.
//
// Matrix-generated tests are grouped: the 6 cells produced by a
// 3-scenario × 2-fault matrix become ONE PlanTest with Kind="fault_matrix"
// and Instances=6, with the participating scenarios and faults captured
// as axes in Compositions. This keeps the rendered tree compact and
// reflects the user's intent (one matrix declaration, six cells) rather
// than dumping the cells flat.
type PlanTest struct {
	Name         string            `json:"name"`
	Kind         PlanTestKind      `json:"kind"`
	Instances    int               `json:"instances"`
	Compositions []PlanComposition `json:"compositions,omitempty"`
	// MatrixCells lists the actual generated cell names for a fault_matrix
	// PlanTest, in deterministic order. Empty for non-matrix tests.
	MatrixCells []string `json:"matrix_cells,omitempty"`
	// Faults names the fault_assumption(s) applied by a direct
	// fault_scenario() declaration (Kind="fault_scenario"). Empty
	// for fault_matrix entries (their faults appear in Compositions).
	Faults []string `json:"faults,omitempty"`
	// Expect, when non-empty, is the predicate name (callable.Name())
	// of the fault_scenario expect= callable or test(expect=) value.
	Expect string `json:"expect,omitempty"`
	// Timeout records test(timeout=) for test_builtin entries, in
	// duration string form ("30s"). Empty for legacy tests, which run
	// under the 3-minute infrastructure ceiling only.
	Timeout string `json:"timeout,omitempty"`
}

// PlanTestKind is the discriminator for PlanTest entries.
type PlanTestKind string

const (
	// KindDef — a `def test_<name>()` function in the spec.
	KindDef PlanTestKind = "def"
	// KindScenario — registered via `scenario("name", fn)`.
	KindScenario PlanTestKind = "scenario"
	// KindFaultScenario — registered via `fault_scenario(name=..., ...)`.
	KindFaultScenario PlanTestKind = "fault_scenario"
	// KindFaultMatrix — produced by `fault_matrix(scenarios=, faults=, ...)`.
	// All cells sharing a matrix collapse into one PlanTest of this kind.
	KindFaultMatrix PlanTestKind = "fault_matrix"
	// KindTestBuiltin — declared via `test(name=, body=, ...)` (RFC-041 §8.6).
	KindTestBuiltin PlanTestKind = "test_builtin"
)

// PlanComposition describes a fan-out axis under a PlanTest. The Kind
// labels the builtin that introduced the axis; Axes carries one entry
// per dimension (a fault_matrix with N scenarios × M faults has two
// axes). The current rc1 emits compositions for fault_matrix only;
// `choose()` (RFC-043) will use the same shape when it lands.
type PlanComposition struct {
	Kind PlanCompositionKind `json:"kind"`
	Axes []PlanAxis          `json:"axes"`
}

// PlanCompositionKind discriminates between matrix-style fan-outs.
type PlanCompositionKind string

const (
	// CompositionFaultMatrix — fault_matrix's scenarios × faults cross product.
	CompositionFaultMatrix PlanCompositionKind = "fault_matrix"
	// CompositionChoose — reserved for RFC-043's choose(); not emitted in v0.13.0-rc1.
	CompositionChoose PlanCompositionKind = "choose"
)

// PlanAxis is one dimension of a composition fan-out. Name is the user-
// visible label ("scenarios", "faults", "retries"); Values are the
// stringified options that the axis ranges over.
type PlanAxis struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// PlanTotals summarizes the cross-product cost across the plan tree.
// rc1 emits Instances only; estimated wall-clock and probability-spread
// counters (RFC-042 §5.9) land with rc2.
type PlanTotals struct {
	Instances int `json:"instances"`
}

// PlanTopology snapshots services. Dependency edges + their coverage
// links live on PlanTree.Coverage (populated by WithCoverage), kept
// separate so callers that only want a service inventory pay nothing
// for the cross-reference walk.
type PlanTopology struct {
	Services []PlanService `json:"services,omitempty"`
}

// PlanService describes one declared service. Interfaces lists the
// named protocol surfaces the service exposes (http, grpc, postgres,
// kafka, etc.); the names are stable across `faultbox plan` invocations
// so the report can build deep links to interface-level coverage.
type PlanService struct {
	Name       string   `json:"name"`
	Interfaces []string `json:"interfaces,omitempty"`
}
