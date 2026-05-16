package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/plan"
)

func TestPlanCmd_TextOutputContainsCorePieces(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.star")
	src := `
svc = service("svc", image="busybox", cmd=["sh","-c","sleep 1"])

def scenario_checkout(): pass
def scenario_browse(): pass

fa1 = fault_assumption("db_down", target=svc, write=deny("EIO"))
fa2 = fault_assumption("db_slow", target=svc, write=delay("100ms"))

fault_matrix(scenarios=[scenario_checkout, scenario_browse], faults=[fa1, fa2])

def test_smoke(): return True
`
	if err := os.WriteFile(spec, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture stdout via a pipe so we can assert on the rendered tree.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	rc := planCmd([]string{spec})

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	if rc != 0 {
		t.Fatalf("planCmd exit = %d, want 0; output=\n%s", rc, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"Spec: " + spec,
		"Determinism: L1 (strict)",
		"Plan tree:",
		"test \"test_matrix_scenario_checkout\"  [fault_matrix]",
		"fault_matrix",
		"faults: [db_down, db_slow]",
		"test \"test_smoke\"  [def]",
		"Total: 5 plan instances",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestPlanCmd_RejectsUnknownFormat(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)
	rc := planCmd([]string{spec, "--format=xml"})
	if rc == 0 {
		t.Error("expected non-zero exit for --format=xml")
	}
}

func TestPlanCmd_JSONFormatRoundTrips(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := planCmd([]string{spec, "--format=json"})
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if rc != 0 {
		t.Fatalf("planCmd --format=json exit = %d; output=\n%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), `"schema_version": 1`) {
		t.Errorf("JSON output missing schema_version: %s", buf.String())
	}
}

func TestPlanCmd_DOTFormatHasDigraph(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := planCmd([]string{spec, "--format=dot"})
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if rc != 0 {
		t.Fatalf("planCmd --format=dot exit = %d; output=\n%s", rc, buf.String())
	}
	if !strings.HasPrefix(buf.String(), "digraph plan {") {
		t.Errorf("DOT output should start with `digraph plan {`: %s", buf.String())
	}
}

func TestPlanCmd_MissingSpecPrintsUsage(t *testing.T) {
	rc := planCmd([]string{})
	if rc == 0 {
		t.Error("expected non-zero exit when no spec provided")
	}
}

func TestPlanCmd_CoverageAddsTable(t *testing.T) {
	spec := writeTempSpec(t, `
db = service("db", image="busybox", cmd=["sh","-c","sleep 1"])
api = service("api", image="busybox", cmd=["sh","-c","sleep 1"], depends_on=[db])
def scenario_x(): pass
db_down = fault_assumption("db_down", target=db, write=deny("EIO"))
fault_scenario("api_db_down", scenario=scenario_x, faults=db_down)
`)
	out := mustCaptureStdout(t, func() int { return planCmd([]string{spec, "--coverage"}) })
	for _, want := range []string{"Coverage:", "✓ api → db", "faulted in: test_api_db_down"} {
		if !strings.Contains(out, want) {
			t.Errorf("coverage output missing %q:\n%s", want, out)
		}
	}
}

func TestPlanCmd_SuggestEmitsStubs(t *testing.T) {
	spec := writeTempSpec(t, `
db = service("db", image="busybox", cmd=["sh","-c","sleep 1"])
api = service("api", image="busybox", cmd=["sh","-c","sleep 1"], depends_on=[db])
def test_x(): return True
`)
	out := mustCaptureStdout(t, func() int { return planCmd([]string{spec, "--suggest"}) })
	if !strings.Contains(out, "# Uncovered edge: api → db") {
		t.Errorf("suggest output missing stub: %s", out)
	}
}

func TestPlanCmd_LLMStrategyReserved(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)
	rc := planCmd([]string{spec, "--suggest", "--strategy=llm"})
	if rc == 0 {
		t.Error("--strategy=llm should be rejected until v0.14.0")
	}
}

func TestPlanCmd_CheckCostExitsTwoOnExceed(t *testing.T) {
	spec := writeTempSpec(t, `
def test_a(): return True
def test_b(): return True
def test_c(): return True
`)
	rc := planCmd([]string{spec, "--check-cost", "--max-instances=1"})
	if rc != 2 {
		t.Errorf("expected exit code 2 when cost gate exceeded, got %d", rc)
	}
}

func TestPlanCmd_CheckCostPassesUnderBudget(t *testing.T) {
	spec := writeTempSpec(t, `def test_a(): return True`)
	rc := planCmd([]string{spec, "--check-cost", "--max-instances=10"})
	if rc != 0 {
		t.Errorf("expected exit 0 under budget, got %d", rc)
	}
}

// N9: boundary — exactly N instances with --max-instances=N must
// pass (the gate is strictly greater-than).
func TestPlanCmd_CheckCostBoundaryExactlyEqual(t *testing.T) {
	spec := writeTempSpec(t, `
def test_a(): return True
def test_b(): return True
def test_c(): return True
`)
	rc := planCmd([]string{spec, "--check-cost", "--max-instances=3"})
	if rc != 0 {
		t.Errorf("expected exit 0 when instance count equals max-instances, got %d", rc)
	}
}

// N1: --check-cost with no --max-instances is a CI footgun; must
// error rather than silently exiting 0.
func TestPlanCmd_CheckCostWithoutBudgetErrors(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)
	rc := planCmd([]string{spec, "--check-cost"})
	if rc == 0 {
		t.Error("expected non-zero exit when --check-cost lacks --max-instances")
	}
}

// mustCaptureStdout runs fn() with stdout piped into a buffer and
// fails the test if fn returns a non-zero exit code. Pass any
// expected exit code via a separate path (call planCmd directly).
//
// Note: this swaps os.Stdout globally and is therefore not safe under
// t.Parallel; callers that want parallelism should refactor planCmd
// to accept an io.Writer. Kept simple here because the plan tests
// are quick and serial.
func mustCaptureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if rc != 0 {
		t.Fatalf("planCmd exit = %d (expected 0); output=\n%s", rc, buf.String())
	}
	return buf.String()
}

// Compile-time assertion: the package imports are wired correctly so a
// renderer regression doesn't slip into rc1.
var _ = plan.SchemaVersion

func writeTempSpec(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.star")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
