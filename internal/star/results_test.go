package star

import (
	"encoding/json"
	"testing"

	"go.starlark.net/starlark"
)

func TestBuildTraceOutputReturnValueString(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:        "test_order_flow",
				Result:      "pass",
				DurationMs:  50,
				ReturnValue: starlark.String("confirmed"),
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if len(out.Tests) != 1 {
		t.Fatalf("expected 1 test, got %d", len(out.Tests))
	}
	if out.Tests[0].ReturnValue != `"confirmed"` {
		t.Errorf("return_value = %q, want %q", out.Tests[0].ReturnValue, `"confirmed"`)
	}
}

func TestBuildTraceOutputReturnValueDict(t *testing.T) {
	d := starlark.NewDict(2)
	d.SetKey(starlark.String("status"), starlark.MakeInt(200))
	d.SetKey(starlark.String("body"), starlark.String("ok"))

	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:        "test_order_flow",
				Result:      "pass",
				DurationMs:  50,
				ReturnValue: d,
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if out.Tests[0].ReturnValue == "" {
		t.Error("expected non-empty return_value for dict return")
	}
	// Verify it's valid content (dict string representation).
	if out.Tests[0].ReturnValue == "None" {
		t.Error("return_value should not be None for dict return")
	}
}

func TestBuildTraceOutputReturnValueNone(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:        "test_happy",
				Result:      "pass",
				DurationMs:  30,
				ReturnValue: starlark.None,
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if out.Tests[0].ReturnValue != "" {
		t.Errorf("return_value should be empty for None, got %q", out.Tests[0].ReturnValue)
	}
}

func TestBuildTraceOutputReturnValueNil(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:       "test_happy",
				Result:     "pass",
				DurationMs: 30,
				// ReturnValue not set (nil)
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if out.Tests[0].ReturnValue != "" {
		t.Errorf("return_value should be empty for nil, got %q", out.Tests[0].ReturnValue)
	}
}

func TestBuildTraceOutputReturnValueJSON(t *testing.T) {
	// Verify return_value appears in JSON output when non-None.
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:        "test_order_flow",
				Result:      "pass",
				DurationMs:  50,
				ReturnValue: starlark.MakeInt(42),
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	tests := parsed["tests"].([]interface{})
	test := tests[0].(map[string]interface{})
	rv, ok := test["return_value"]
	if !ok {
		t.Error("return_value field missing from JSON")
	}
	if rv != "42" {
		t.Errorf("return_value = %v, want \"42\"", rv)
	}
}

func TestBuildTraceOutputMatrixSection(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 200,
		Pass:       4,
		Tests: []TestResult{
			{Name: "test_matrix_order_flow_db_down", Result: "pass", DurationMs: 10, Matrix: &MatrixInfo{ScenarioName: "order_flow", FaultName: "db_down"}},
			{Name: "test_matrix_order_flow_disk_full", Result: "pass", DurationMs: 8, Matrix: &MatrixInfo{ScenarioName: "order_flow", FaultName: "disk_full"}},
			{Name: "test_matrix_health_check_db_down", Result: "pass", DurationMs: 5, Matrix: &MatrixInfo{ScenarioName: "health_check", FaultName: "db_down"}},
			{Name: "test_matrix_health_check_disk_full", Result: "fail", DurationMs: 4, Reason: "assert failed", Matrix: &MatrixInfo{ScenarioName: "health_check", FaultName: "disk_full"}},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if out.Matrix == nil {
		t.Fatal("expected matrix section in trace output")
	}

	m := out.Matrix
	if len(m.Scenarios) != 2 {
		t.Errorf("expected 2 scenarios, got %d", len(m.Scenarios))
	}
	if len(m.Faults) != 2 {
		t.Errorf("expected 2 faults, got %d", len(m.Faults))
	}
	if m.Total != 4 {
		t.Errorf("total = %d, want 4", m.Total)
	}
	if m.Passed != 3 {
		t.Errorf("passed = %d, want 3", m.Passed)
	}
	if m.Failed != 1 {
		t.Errorf("failed = %d, want 1", m.Failed)
	}
	if len(m.Cells) != 4 {
		t.Fatalf("expected 4 cells, got %d", len(m.Cells))
	}

	// Verify one cell.
	found := false
	for _, c := range m.Cells {
		if c.Scenario == "health_check" && c.Fault == "disk_full" {
			found = true
			if c.Passed {
				t.Error("health_check × disk_full should be failed")
			}
			if c.Reason != "assert failed" {
				t.Errorf("reason = %q, want 'assert failed'", c.Reason)
			}
		}
	}
	if !found {
		t.Error("missing health_check × disk_full cell")
	}
}

func TestBuildTraceOutputMatrixJSON(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       2,
		Tests: []TestResult{
			{Name: "test_matrix_a_x", Result: "pass", DurationMs: 10, Matrix: &MatrixInfo{ScenarioName: "a", FaultName: "x"}},
			{Name: "test_matrix_a_y", Result: "pass", DurationMs: 20, Matrix: &MatrixInfo{ScenarioName: "a", FaultName: "y"}},
		},
	}

	out := BuildTraceOutput("test.star", result)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	matrix, ok := parsed["matrix"]
	if !ok {
		t.Fatal("matrix field missing from JSON")
	}
	m := matrix.(map[string]interface{})
	if m["total"].(float64) != 2 {
		t.Errorf("total = %v, want 2", m["total"])
	}
}

func TestBuildTraceOutputNoMatrixSection(t *testing.T) {
	// Non-matrix tests should not produce a matrix section.
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{Name: "test_happy", Result: "pass", DurationMs: 50},
		},
	}

	out := BuildTraceOutput("test.star", result)
	if out.Matrix != nil {
		t.Error("expected nil matrix section for non-matrix tests")
	}

	// Verify omitted from JSON.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)
	if _, ok := parsed["matrix"]; ok {
		t.Error("matrix should be omitted from JSON when no matrix tests")
	}
}

func TestBuildTraceOutputReturnValueOmittedFromJSONWhenNone(t *testing.T) {
	// Verify return_value is omitted from JSON when None (omitempty).
	result := &SuiteResult{
		DurationMs: 100,
		Pass:       1,
		Tests: []TestResult{
			{
				Name:        "test_happy",
				Result:      "pass",
				DurationMs:  30,
				ReturnValue: starlark.None,
			},
		},
	}

	out := BuildTraceOutput("test.star", result)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	tests := parsed["tests"].([]interface{})
	test := tests[0].(map[string]interface{})
	if _, ok := test["return_value"]; ok {
		t.Error("return_value should be omitted from JSON when None")
	}
}
