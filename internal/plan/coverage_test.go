package plan

import (
	"bytes"
	"strings"
	"testing"
)

func TestWithCoverage_MarksFaultedAndUnfaultedEdges(t *testing.T) {
	rt := newRuntime(t)
	src := `
db = service("db",
    image = "busybox",
)
redis = service("redis",
    image = "busybox",
)
api = service("api",
    image = "busybox",
    depends_on = [db, redis],
)

def scenario_x(): pass

db_down = fault_assumption("db_down", target=db, write=deny("EIO"))

fault_scenario("api_db_down", scenario=scenario_x, faults=db_down)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	pt := Enumerate(rt)
	if err := WithCoverage(pt, rt); err != nil {
		t.Fatalf("WithCoverage: %v", err)
	}
	if pt.Coverage == nil {
		t.Fatal("Coverage should be populated")
	}

	var dbEdge, redisEdge *PlanEdge
	for i, e := range pt.Coverage.Edges {
		if e.To == "db" {
			dbEdge = &pt.Coverage.Edges[i]
		}
		if e.To == "redis" {
			redisEdge = &pt.Coverage.Edges[i]
		}
	}
	if dbEdge == nil || redisEdge == nil {
		t.Fatalf("expected db and redis edges; got %+v", pt.Coverage.Edges)
	}
	if len(dbEdge.FaultTests) == 0 || dbEdge.FaultTests[0] != "test_api_db_down" {
		t.Errorf("db edge should be covered by test_api_db_down; got %v", dbEdge.FaultTests)
	}
	if len(redisEdge.FaultTests) != 0 {
		t.Errorf("redis edge should be uncovered; got %v", redisEdge.FaultTests)
	}
	if pt.Coverage.UncoveredEdges != 1 {
		t.Errorf("UncoveredEdges = %d, want 1", pt.Coverage.UncoveredEdges)
	}
}

func TestWithCoverage_MatrixCellsAttributedToCollapsedTest(t *testing.T) {
	rt := newRuntime(t)
	src := `
db = service("db", image="busybox")
api = service("api", image="busybox", depends_on=[db])

def scenario_checkout(): pass
def scenario_browse(): pass

fa1 = fault_assumption("db_down", target=db, write=deny("EIO"))
fa2 = fault_assumption("db_slow", target=db, write=delay("100ms"))

fault_matrix(scenarios=[scenario_checkout, scenario_browse], faults=[fa1, fa2])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	pt := Enumerate(rt)
	if err := WithCoverage(pt, rt); err != nil {
		t.Fatalf("WithCoverage: %v", err)
	}

	// Build a set of PlanTest names so we can verify coverage links
	// point at rows the user actually sees.
	visible := map[string]bool{}
	for _, tr := range pt.Tests {
		visible[tr.Name] = true
	}

	var dbEdge *PlanEdge
	for i, e := range pt.Coverage.Edges {
		if e.To == "db" {
			dbEdge = &pt.Coverage.Edges[i]
		}
	}
	if dbEdge == nil {
		t.Fatalf("expected db edge; got %+v", pt.Coverage.Edges)
	}
	if len(dbEdge.FaultTests) == 0 {
		t.Fatal("db edge should be covered by the fault_matrix")
	}
	for _, name := range dbEdge.FaultTests {
		if !visible[name] {
			t.Errorf("coverage attribution %q does not match any PlanTest.Name; visible=%v", name, keys(visible))
		}
	}
	// Two scenarios × two faults = 4 cells; both scenarios should attribute.
	for _, want := range []string{"test_matrix_scenario_browse", "test_matrix_scenario_checkout"} {
		found := false
		for _, n := range dbEdge.FaultTests {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("db edge FaultTests missing %q; got %v", want, dbEdge.FaultTests)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWriteSuggestions_EmitsStubsForUncovered(t *testing.T) {
	pt := &PlanTree{
		Coverage: &PlanCoverage{
			Edges: []PlanEdge{
				{From: "api", To: "db", Protocol: "postgres", FaultTests: []string{"test_x"}},
				{From: "api", To: "redis", Protocol: "redis"},
				{From: "worker", To: "kafka", Protocol: "kafka"},
			},
			UncoveredEdges: 2,
		},
	}
	var buf bytes.Buffer
	n := WriteSuggestions(&buf, pt)
	if n != 2 {
		t.Errorf("WriteSuggestions returned %d, want 2", n)
	}
	out := buf.String()
	for _, want := range []string{
		"# Uncovered edge: api → redis",
		"# Uncovered edge: worker → kafka",
		"redis_unavailable",
		"kafka_unavailable",
		"fault_scenario(\"api_when_redis_down\"",
		"connect = deny(\"ECONNREFUSED\")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "# Uncovered edge: api → db") {
		t.Error("covered edge should not appear in suggestions")
	}
}

func TestWriteSuggestions_NoUncoveredEdges(t *testing.T) {
	pt := &PlanTree{
		Coverage: &PlanCoverage{
			Edges: []PlanEdge{{From: "a", To: "b", FaultTests: []string{"test_x"}}},
		},
	}
	var buf bytes.Buffer
	n := WriteSuggestions(&buf, pt)
	if n != 0 {
		t.Errorf("expected 0 suggestions when all covered, got %d", n)
	}
	if !strings.Contains(buf.String(), "All dependency edges have at least one fault test") {
		t.Errorf("expected all-covered message; got %s", buf.String())
	}
}

func TestWriteCoverageText_FormatsTable(t *testing.T) {
	pt := &PlanTree{
		Topology: PlanTopology{Services: []PlanService{{Name: "api"}, {Name: "db"}}},
		Coverage: &PlanCoverage{
			Edges: []PlanEdge{
				{From: "api", To: "db", FaultTests: []string{"test_x"}},
				{From: "api", To: "redis"},
			},
			UncoveredEdges: 1,
		},
	}
	var buf bytes.Buffer
	uncov := WriteCoverageText(&buf, pt)
	if uncov != 1 {
		t.Errorf("WriteCoverageText returned %d, want 1", uncov)
	}
	out := buf.String()
	for _, want := range []string{
		"Coverage:",
		"2 services declared",
		"2 dependency edges",
		"✓ api → db",
		"⚠ api → redis",
		"1 edge without fault coverage",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in coverage text:\n%s", want, out)
		}
	}
}
