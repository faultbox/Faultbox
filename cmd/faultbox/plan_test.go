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

func TestPlanCmd_DeferredFormatsErrorClearly(t *testing.T) {
	spec := writeTempSpec(t, `def test_x(): return True`)
	for _, f := range []string{"json", "dot"} {
		rc := planCmd([]string{spec, "--format=" + f})
		if rc == 0 {
			t.Errorf("--format=%s should fail until PR 3 lands", f)
		}
	}
}

func TestPlanCmd_MissingSpecPrintsUsage(t *testing.T) {
	rc := planCmd([]string{})
	if rc == 0 {
		t.Error("expected non-zero exit when no spec provided")
	}
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
