package plan

import (
	"sort"
	"strings"

	"github.com/faultbox/Faultbox/internal/star"
)

// Enumerate walks the loaded runtime and produces a deterministic plan
// tree. No services launch, no Starlark executes — every value it
// reads is already on rt by the time LoadFile / LoadString returned.
//
// The returned tree is the same on every call against the same loaded
// runtime: it walks maps via sorted-key iteration and never consults
// any source of nondeterminism. RFC-040 L0 guarantees the same loaded
// runtime for `(spec, seed)` so consecutive `faultbox plan` invocations
// over the same spec produce byte-identical JSON.
//
// rt must be a fully-loaded runtime — call after LoadFile / LoadString
// returns. Passing nil returns a zero-valued PlanTree.
func Enumerate(rt *star.Runtime) *PlanTree {
	pt := &PlanTree{
		SchemaVersion: SchemaVersion,
	}
	if rt == nil {
		return pt
	}

	pt.SpecPath = rt.RootSpecPath()
	pt.Seed = rt.PlanSeed()

	level, runtimeName, strict, explicit := rt.Determinism()
	pt.Determinism = PlanDeterminism{
		Level:    level,
		Runtime:  runtimeName,
		Strict:   strict,
		Explicit: explicit,
	}

	pt.Tests = buildTests(rt)
	pt.Totals = computeTotals(pt.Tests)
	pt.Topology = buildTopology(rt)
	return pt
}

// buildTests merges the runtime's four overlapping test sources (def
// functions, scenario() registrations, fault_scenario / fault_matrix
// cells, and test() builtin configs) into a single sorted slice of
// PlanTest. The merge rules mirror DiscoverTests' precedence: scenario
// and fault_scenario entries only appear when no `def test_<name>()`
// has the same name (the def wins because DiscoverTests adds the
// scenario as a fallback, never overwriting).
func buildTests(rt *star.Runtime) []PlanTest {
	defNames := defTestNames(rt)
	scenarioByName := indexScenarios(rt)
	cellsByMatrix, freeScenarios := groupFaultScenarios(rt.FaultScenarios())
	testConfigs := rt.TestConfigs()

	// Deduplicate via a seen set keyed by the discoverable test name
	// (always prefixed "test_"). Insertion order: def > test_builtin >
	// fault_matrix > fault_scenario > scenario(). This is informational
	// — we sort the final slice — but it matches DiscoverTests for
	// debugging predictability.
	seen := make(map[string]bool)
	var tests []PlanTest

	for _, name := range sortedStrings(defNames) {
		tests = append(tests, PlanTest{
			Name:      name,
			Kind:      KindDef,
			Instances: 1,
		})
		seen[name] = true
	}

	for _, fullName := range sortedKeys(testConfigs) {
		if seen[fullName] {
			continue
		}
		cfg := testConfigs[fullName]
		entry := PlanTest{
			Name:      fullName,
			Kind:      KindTestBuiltin,
			Instances: 1,
		}
		if cfg.Expect != nil {
			entry.Expect = cfg.Expect.Type()
		}
		if cfg.Timeout > 0 {
			entry.Timeout = cfg.Timeout.String()
		}
		tests = append(tests, entry)
		seen[fullName] = true
	}

	for _, scenarioName := range sortedKeys(cellsByMatrix) {
		cells := cellsByMatrix[scenarioName]
		testName := "test_matrix_" + scenarioName
		if seen[testName] {
			continue
		}
		entry := PlanTest{
			Name:      testName,
			Kind:      KindFaultMatrix,
			Instances: len(cells),
		}
		entry.Compositions = []PlanComposition{
			matrixComposition(scenarioName, cells),
		}
		entry.MatrixCells = cellNames(cells)
		entry.Expect = matrixExpectName(cells)
		tests = append(tests, entry)
		seen[testName] = true
	}

	for _, fsName := range sortedKeys(freeScenarios) {
		fs := freeScenarios[fsName]
		testName := "test_" + fs.Name
		if seen[testName] {
			continue
		}
		entry := PlanTest{
			Name:      testName,
			Kind:      KindFaultScenario,
			Instances: 1,
			Faults:    faultNames(fs.Faults),
		}
		if fs.Expect != nil {
			entry.Expect = fs.Expect.Name()
		}
		tests = append(tests, entry)
		seen[testName] = true
	}

	for _, sName := range sortedStrings(sortedKeys(scenarioByName)) {
		testName := "test_" + sName
		if seen[testName] {
			continue
		}
		tests = append(tests, PlanTest{
			Name:      testName,
			Kind:      KindScenario,
			Instances: 1,
		})
		seen[testName] = true
	}

	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	return tests
}

// matrixComposition collapses a slice of fault_matrix cells into the
// canonical {scenarios, faults} axis pair. The values are deduplicated
// and sorted so two equivalent matrices (even if registered in
// different orders) produce identical plan output.
func matrixComposition(scenarioName string, cells []*star.FaultScenarioDef) PlanComposition {
	scenarioSet := make(map[string]struct{})
	faultSet := make(map[string]struct{})
	for _, c := range cells {
		if c.Matrix != nil {
			scenarioSet[c.Matrix.ScenarioName] = struct{}{}
			scenarioSet[scenarioName] = struct{}{}
			faultSet[c.Matrix.FaultName] = struct{}{}
		}
	}
	return PlanComposition{
		Kind: CompositionFaultMatrix,
		Axes: []PlanAxis{
			{Name: "scenarios", Values: sortedSetValues(scenarioSet)},
			{Name: "faults", Values: sortedSetValues(faultSet)},
		},
	}
}

// matrixExpectName returns the expect predicate name shared by the
// matrix cells, or "" if cells disagree (overrides=) — surfaced
// distinctly so the plan reader notices.
func matrixExpectName(cells []*star.FaultScenarioDef) string {
	var name string
	for _, c := range cells {
		if c.Expect == nil {
			continue
		}
		n := c.Expect.Name()
		if name == "" {
			name = n
			continue
		}
		if name != n {
			return "(mixed)"
		}
	}
	return name
}

// cellNames returns the sorted set of matrix cell test-names. Used by
// the report's drill-down: clicking a matrix node shows the cells it
// produced so users see exactly which (scenario, fault) ran.
func cellNames(cells []*star.FaultScenarioDef) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, "test_"+c.Name)
	}
	sort.Strings(out)
	return out
}

// faultNames extracts the user-visible fault_assumption names from a
// scenario's fault list. Stable order for deterministic output.
func faultNames(faults []*star.FaultAssumptionDef) []string {
	if len(faults) == 0 {
		return nil
	}
	out := make([]string, 0, len(faults))
	for _, f := range faults {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// defTestNames discovers `def test_*()` callables already loaded into
// the runtime. It walks the globals dict directly (via the read-only
// GlobalCallableNames accessor) so Enumerate stays a pure read —
// DiscoverTests would have written scenario / fault_scenario wrappers
// back into globals as a side effect, breaking Enumerate's "no
// mutation" promise.
//
// scenario()/fault_scenario()/test() entries are filtered out so this
// returns only `def test_*()` functions; the other kinds get their
// own PlanTest entries via buildTests's other passes.
func defTestNames(rt *star.Runtime) []string {
	scenarioNames := indexScenarios(rt)
	_, freeScenarios := groupFaultScenarios(rt.FaultScenarios())
	testConfigs := rt.TestConfigs()

	var defs []string
	for _, name := range rt.GlobalCallableNames() {
		if !strings.HasPrefix(name, "test_") {
			continue
		}
		// matrix cells (test_matrix_*) and test()-builtin entries
		// aren't `def test_*()` functions even though their wrappers
		// may end up in globals later — exclude them here.
		if strings.HasPrefix(name, "test_matrix_") {
			continue
		}
		if _, ok := testConfigs[name]; ok {
			continue
		}
		if _, ok := scenarioNames[strings.TrimPrefix(name, "test_")]; ok {
			continue
		}
		if _, ok := freeScenarios[strings.TrimPrefix(name, "test_")]; ok {
			continue
		}
		defs = append(defs, name)
	}
	return defs
}

func indexScenarios(rt *star.Runtime) map[string]star.ScenarioRegistration {
	out := make(map[string]star.ScenarioRegistration)
	for _, s := range rt.Scenarios() {
		out[s.Name] = s
	}
	return out
}

// groupFaultScenarios splits the runtime's fault_scenario map into
// (matrix-grouped) and (free) entries. A scenario with Matrix != nil
// is grouped under its Matrix.ScenarioName; free scenarios keep their
// own name as the map key.
func groupFaultScenarios(scenarios map[string]*star.FaultScenarioDef) (
	cellsByMatrix map[string][]*star.FaultScenarioDef,
	free map[string]*star.FaultScenarioDef,
) {
	cellsByMatrix = make(map[string][]*star.FaultScenarioDef)
	free = make(map[string]*star.FaultScenarioDef)
	for _, fs := range scenarios {
		if fs == nil {
			continue
		}
		if fs.Matrix != nil {
			cellsByMatrix[fs.Matrix.ScenarioName] = append(cellsByMatrix[fs.Matrix.ScenarioName], fs)
			continue
		}
		free[fs.Name] = fs
	}
	// Sort each matrix's cells deterministically so axis output is stable.
	for k := range cellsByMatrix {
		cells := cellsByMatrix[k]
		sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
		cellsByMatrix[k] = cells
	}
	return cellsByMatrix, free
}

func buildTopology(rt *star.Runtime) PlanTopology {
	svcs := rt.Services()
	out := make([]PlanService, 0, len(svcs))
	for _, svc := range svcs {
		ps := PlanService{Name: svc.Name}
		for name := range svc.Interfaces {
			ps.Interfaces = append(ps.Interfaces, name)
		}
		sort.Strings(ps.Interfaces)
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return PlanTopology{Services: out}
}

func computeTotals(tests []PlanTest) PlanTotals {
	n := 0
	for _, t := range tests {
		n += t.Instances
	}
	return PlanTotals{Instances: n}
}

// sortedKeys is the generic map-key sorter used throughout this file.
// Pulled out for testability; Go generics keep the boilerplate down.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedSetValues(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
