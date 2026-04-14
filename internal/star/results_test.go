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
