package plan

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/star"
)

func newRuntime(t *testing.T) *star.Runtime {
	t.Helper()
	return star.New(slog.New(slog.NewTextHandler(testWriter{t}, nil)))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func TestEnumerate_NilRuntime(t *testing.T) {
	pt := Enumerate(nil)
	if pt == nil {
		t.Fatal("Enumerate(nil) returned nil")
	}
	if pt.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", pt.SchemaVersion, SchemaVersion)
	}
	if pt.Totals.Instances != 0 || len(pt.Tests) != 0 {
		t.Errorf("zero-value tree should have no tests; got %+v", pt)
	}
}

func TestEnumerate_DefTestOnly(t *testing.T) {
	rt := newRuntime(t)
	src := `
def test_foo():
    return True

def test_bar():
    return True
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	pt := Enumerate(rt)
	if len(pt.Tests) != 2 {
		t.Fatalf("expected 2 tests, got %d: %+v", len(pt.Tests), pt.Tests)
	}
	for _, tr := range pt.Tests {
		if tr.Kind != KindDef {
			t.Errorf("expected KindDef, got %q for %s", tr.Kind, tr.Name)
		}
		if tr.Instances != 1 {
			t.Errorf("def test should have 1 instance, got %d for %s", tr.Instances, tr.Name)
		}
	}
	if pt.Totals.Instances != 2 {
		t.Errorf("Totals.Instances = %d, want 2", pt.Totals.Instances)
	}
	// Tests must be sorted by name — the plan-tree output is meant to
	// be byte-stable for diffing across runs.
	if pt.Tests[0].Name != "test_bar" || pt.Tests[1].Name != "test_foo" {
		t.Errorf("tests not sorted by name: %+v", pt.Tests)
	}
}

func TestEnumerate_FaultMatrixCollapsesCells(t *testing.T) {
	rt := newRuntime(t)
	src := `
svc = service("svc", image = "busybox")

def scenario_a(): pass
def scenario_b(): pass

fa_x = fault_assumption("fx", target = svc, write = deny("EIO"))
fa_y = fault_assumption("fy", target = svc, write = deny("EIO"))

fault_matrix(
    scenarios = [scenario_a, scenario_b],
    faults    = [fa_x, fa_y],
)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	pt := Enumerate(rt)
	// Expect 2 matrix tests (one per scenario), each with 2 cells.
	matrixTests := 0
	totalInstances := 0
	for _, tr := range pt.Tests {
		if tr.Kind != KindFaultMatrix {
			continue
		}
		matrixTests++
		totalInstances += tr.Instances
		if tr.Instances != 2 {
			t.Errorf("expected 2 cells per matrix scenario, got %d for %s", tr.Instances, tr.Name)
		}
		if len(tr.Compositions) != 1 {
			t.Fatalf("expected one composition entry, got %d", len(tr.Compositions))
		}
		comp := tr.Compositions[0]
		if comp.Kind != CompositionFaultMatrix {
			t.Errorf("composition kind = %q, want fault_matrix", comp.Kind)
		}
		// Faults axis must list fx and fy.
		var faultAxis PlanAxis
		for _, ax := range comp.Axes {
			if ax.Name == "faults" {
				faultAxis = ax
			}
		}
		if got := strings.Join(faultAxis.Values, ","); got != "fx,fy" {
			t.Errorf("faults axis values = %q, want \"fx,fy\"", got)
		}
		if len(tr.MatrixCells) != 2 {
			t.Errorf("MatrixCells len = %d, want 2", len(tr.MatrixCells))
		}
	}
	if matrixTests != 2 {
		t.Errorf("expected 2 matrix tests (one per scenario), got %d", matrixTests)
	}
	if totalInstances != 4 {
		t.Errorf("totalInstances = %d, want 4 (2 scenarios × 2 faults)", totalInstances)
	}
}

func TestEnumerate_FaultScenarioStandalone(t *testing.T) {
	rt := newRuntime(t)
	src := `
svc = service("svc", image = "busybox")

def scenario_a(): pass

fa = fault_assumption("fx", target = svc, write = deny("EIO"))

fault_scenario(
    "checkout_db_down",
    scenario = scenario_a,
    faults   = fa,
)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	pt := Enumerate(rt)

	var fs *PlanTest
	for i, tr := range pt.Tests {
		if tr.Kind == KindFaultScenario {
			fs = &pt.Tests[i]
			break
		}
	}
	if fs == nil {
		t.Fatalf("expected a fault_scenario plan entry; got %+v", pt.Tests)
	}
	if fs.Name != "test_checkout_db_down" {
		t.Errorf("name = %q", fs.Name)
	}
	if fs.Instances != 1 {
		t.Errorf("instances = %d, want 1", fs.Instances)
	}
	if len(fs.Faults) != 1 || fs.Faults[0] != "fx" {
		t.Errorf("Faults = %v, want [fx]", fs.Faults)
	}
}

func TestEnumerate_TestBuiltinSurfaced(t *testing.T) {
	rt := newRuntime(t)
	src := `
def body(): return None
test("ordering_invariant", body = body, timeout = "45s")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	pt := Enumerate(rt)
	if len(pt.Tests) != 1 {
		t.Fatalf("expected 1 test, got %d: %+v", len(pt.Tests), pt.Tests)
	}
	tr := pt.Tests[0]
	if tr.Kind != KindTestBuiltin {
		t.Errorf("Kind = %q, want test_builtin", tr.Kind)
	}
	if tr.Name != "test_ordering_invariant" {
		t.Errorf("Name = %q", tr.Name)
	}
	if tr.Timeout != "45s" {
		t.Errorf("Timeout = %q, want 45s", tr.Timeout)
	}
}

func TestEnumerate_DeterminismMetadata(t *testing.T) {
	rt := newRuntime(t)
	// Default — spec doesn't call determinism().
	if err := rt.LoadString("spec.star", `def test_x(): return None`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	pt := Enumerate(rt)
	if pt.Determinism.Level != "L1" {
		t.Errorf("default level = %q, want L1", pt.Determinism.Level)
	}
	if !pt.Determinism.Strict {
		t.Error("default strict should be true")
	}
	if pt.Determinism.Explicit {
		t.Error("Explicit should be false when spec did not call determinism()")
	}
}

func TestEnumerate_TopologyServices(t *testing.T) {
	rt := newRuntime(t)
	src := `
api = service("api",
    interface("public", "http", 80),
    interface("internal", "http", 8080),
    image = "busybox",
)
db = service("db", image = "busybox")
def test_x(): return None
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	pt := Enumerate(rt)
	if len(pt.Topology.Services) != 2 {
		t.Fatalf("services = %d, want 2: %+v", len(pt.Topology.Services), pt.Topology.Services)
	}
	if pt.Topology.Services[0].Name != "api" {
		t.Errorf("first service = %q, want api", pt.Topology.Services[0].Name)
	}
	if got := strings.Join(pt.Topology.Services[0].Interfaces, ","); got != "internal,public" {
		t.Errorf("api interfaces = %q, want internal,public", got)
	}
}

// N2: byte stability through the full Enumerate → WriteJSON path.
// TestWriteJSON_ByteStableAcrossCalls covers the encoder; this test
// covers that the encoder's input is itself byte-stable when produced
// from a live runtime.
func TestEnumerate_ByteStableAcrossCallsViaJSON(t *testing.T) {
	rt := newRuntime(t)
	src := `
svc = service("svc", image="busybox")
def scenario_a(): pass
def scenario_b(): pass
fa1 = fault_assumption("f1", target=svc, write=deny("EIO"))
fa2 = fault_assumption("f2", target=svc, write=delay("100ms"))
fault_matrix(scenarios=[scenario_a, scenario_b], faults=[fa1, fa2])
def test_x(): return None
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	var a, b []byte
	{
		var buf strings.Builder
		_ = WriteJSON(&stringWriter{&buf}, Enumerate(rt))
		a = []byte(buf.String())
	}
	{
		var buf strings.Builder
		_ = WriteJSON(&stringWriter{&buf}, Enumerate(rt))
		b = []byte(buf.String())
	}
	if string(a) != string(b) {
		t.Errorf("Enumerate→WriteJSON not byte-stable; diff:\nfirst=\n%s\nsecond=\n%s", a, b)
	}
}

// stringWriter adapts a strings.Builder to io.Writer.
type stringWriter struct{ b *strings.Builder }

func (s *stringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }

func TestEnumerate_DeterministicAcrossCalls(t *testing.T) {
	rt := newRuntime(t)
	src := `
svc = service("svc", image="busybox")
def scenario_a(): pass
def scenario_b(): pass
fa1 = fault_assumption("f1", target=svc, write=deny("EIO"))
fa2 = fault_assumption("f2", target=svc, write=deny("EIO"))
fault_matrix(scenarios=[scenario_a, scenario_b], faults=[fa1, fa2])
def test_x(): return None
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	first := Enumerate(rt)
	second := Enumerate(rt)
	// Use go-cmp-style equality via field counts; deep-equal is enough.
	if first.Totals != second.Totals || len(first.Tests) != len(second.Tests) {
		t.Fatalf("plan trees differ: first=%+v second=%+v", first.Totals, second.Totals)
	}
	for i := range first.Tests {
		if first.Tests[i].Name != second.Tests[i].Name {
			t.Errorf("test[%d] differs: %q vs %q", i, first.Tests[i].Name, second.Tests[i].Name)
		}
		if first.Tests[i].Instances != second.Tests[i].Instances {
			t.Errorf("test[%d] instance count differs: %d vs %d", i, first.Tests[i].Instances, second.Tests[i].Instances)
		}
	}
}
