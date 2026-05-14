package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteJSON_ContainsRequiredFields(t *testing.T) {
	pt := samplePlanTree()
	var buf bytes.Buffer
	if err := WriteJSON(&buf, pt); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("JSON output should end with a newline (jq compat)")
	}
	var roundTrip PlanTree
	if err := json.Unmarshal(buf.Bytes(), &roundTrip); err != nil {
		t.Fatalf("round-trip unmarshal: %v\n---\n%s", err, buf.String())
	}
	if roundTrip.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d after round trip, want %d", roundTrip.SchemaVersion, SchemaVersion)
	}
	if len(roundTrip.Tests) != len(pt.Tests) {
		t.Errorf("tests len = %d after round trip, want %d", len(roundTrip.Tests), len(pt.Tests))
	}
	if roundTrip.Totals.Instances != pt.Totals.Instances {
		t.Errorf("totals.instances = %d, want %d", roundTrip.Totals.Instances, pt.Totals.Instances)
	}
}

func TestWriteJSON_ByteStableAcrossCalls(t *testing.T) {
	pt := samplePlanTree()
	var a, b bytes.Buffer
	if err := WriteJSON(&a, pt); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b, pt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("WriteJSON produced different bytes on consecutive calls — Enumerate must be stable")
	}
}

func TestWriteDOT_WellFormed(t *testing.T) {
	pt := samplePlanTree()
	var buf bytes.Buffer
	if err := WriteDOT(&buf, pt); err != nil {
		t.Fatalf("WriteDOT: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "digraph plan {") {
		t.Error("DOT output should start with `digraph plan {`")
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "}") {
		t.Error("DOT output should end with `}`")
	}
	for _, want := range []string{
		"spec [",
		"test_alpha",
		"test_matrix_demo",
		"fault_matrix",
		"faults:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DOT output missing %q\n---\n%s", want, out)
		}
	}
}

// samplePlanTree builds a small fixture without going through Enumerate
// so render tests don't depend on the runtime. Mirrors the shape
// Enumerate would produce for one def test + one fault_matrix.
func samplePlanTree() *PlanTree {
	return &PlanTree{
		SchemaVersion: SchemaVersion,
		SpecPath:      "/tmp/spec.star",
		Determinism: PlanDeterminism{
			Level:    "L1",
			Runtime:  "default",
			Strict:   true,
			Explicit: false,
		},
		Tests: []PlanTest{
			{Name: "test_alpha", Kind: KindDef, Instances: 1},
			{
				Name:      "test_matrix_demo",
				Kind:      KindFaultMatrix,
				Instances: 4,
				Compositions: []PlanComposition{
					{
						Kind: CompositionFaultMatrix,
						Axes: []PlanAxis{
							{Name: "scenarios", Values: []string{"demo"}},
							{Name: "faults", Values: []string{"db_down", "db_slow"}},
						},
					},
				},
				MatrixCells: []string{"test_matrix_demo_db_down", "test_matrix_demo_db_slow"},
				Expect:      "expect_ok",
			},
		},
		Totals: PlanTotals{Instances: 5},
		Topology: PlanTopology{Services: []PlanService{
			{Name: "svc", Interfaces: []string{"http"}},
		}},
	}
}
