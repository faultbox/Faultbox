package generate

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/star"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestAnalyzeTopology(t *testing.T) {
	rt := star.New(testLogger())
	err := rt.LoadString("test.star", `
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
)

cache = service("cache", "/tmp/mock-cache",
    interface("main", "tcp", 6379),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"DB_ADDR": "localhost:5432"},
    depends_on = [db, cache],
)

def order_flow():
    pass

scenario(order_flow)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	a, err := Analyze(rt)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(a.Services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(a.Services))
	}

	// Should have edges from api → db and api → cache.
	if len(a.Edges) < 2 {
		t.Fatalf("expected at least 2 edges, got %d", len(a.Edges))
	}

	// Should have 1 scenario.
	if len(a.Scenarios) != 1 || a.Scenarios[0].Name != "order_flow" {
		t.Fatalf("expected 1 scenario 'order_flow', got %v", a.Scenarios)
	}
}

func TestBuildMatrix(t *testing.T) {
	a := &Analysis{
		Services: []ServiceInfo{
			{Name: "api", Protocol: "http"},
			{Name: "db", Protocol: "tcp"},
		},
		Edges: []DependencyEdge{
			{From: "api", To: "db", Via: "depends_on", Protocol: "tcp"},
		},
		Scenarios: []ScenarioInfo{
			{Name: "order_flow"},
		},
	}

	mutations := BuildMatrix(a)

	if len(mutations) == 0 {
		t.Fatal("expected mutations, got 0")
	}

	// Should have network + disk mutations.
	hasNetwork := false
	hasDisk := false
	for _, m := range mutations {
		if m.Category == "network" {
			hasNetwork = true
		}
		if m.Category == "disk" {
			hasDisk = true
		}
	}
	if !hasNetwork {
		t.Error("expected network mutations")
	}
	if !hasDisk {
		t.Error("expected disk mutations")
	}

	// All mutations should reference the scenario.
	for _, m := range mutations {
		if m.Scenario != "order_flow" {
			t.Errorf("mutation %s has scenario=%q, want order_flow", m.Name, m.Scenario)
		}
	}
}

func TestGenerate(t *testing.T) {
	a := &Analysis{
		Services: []ServiceInfo{
			{Name: "api", Protocol: "http"},
			{Name: "db", Protocol: "tcp"},
		},
		Edges: []DependencyEdge{
			{From: "api", To: "db", Via: "depends_on", Protocol: "tcp"},
		},
		Scenarios: []ScenarioInfo{
			{Name: "order_flow"},
		},
	}

	mutations := BuildMatrix(a)
	code := Generate(mutations, a, GenerateOpts{Source: "faultbox.star"})

	// Should contain load() statement.
	if !strings.Contains(code, `load("faultbox.star"`) {
		t.Error("expected load() statement")
	}

	// Should contain test functions.
	if !strings.Contains(code, "def test_gen_order_flow_db_down") {
		t.Error("expected test_gen_order_flow_db_down")
	}

	// Should reference the scenario function.
	if !strings.Contains(code, "run=order_flow") {
		t.Error("expected run=order_flow in generated code")
	}

	// Should contain fault() calls.
	if !strings.Contains(code, "fault(") {
		t.Error("expected fault() calls")
	}

	// Should contain partition() call.
	if !strings.Contains(code, "partition(") {
		t.Error("expected partition() call")
	}

	// Should NOT contain any assertions (user adds them).
	if strings.Contains(code, "assert_") {
		t.Error("generated code should not contain assertions")
	}
}

func TestDryRun(t *testing.T) {
	a := &Analysis{
		Edges: []DependencyEdge{
			{From: "api", To: "db"},
		},
		Scenarios: []ScenarioInfo{
			{Name: "order_flow"},
		},
	}

	mutations := BuildMatrix(a)
	summary := DryRun(mutations, a)

	if !strings.Contains(summary, "order_flow") {
		t.Error("expected scenario name in dry run")
	}
	if !strings.Contains(summary, "Total:") {
		t.Error("expected total count in dry run")
	}
}
