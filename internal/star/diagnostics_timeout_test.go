package star

import "testing"

// diagCodes extracts diagnostic codes for terse assertions.
func diagCodes(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(diags []Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestTimeoutWithNoFaultsIsNotBlamedOnFaults is F-5.
//
// The reporter's spec declared zero faults. An image that would not pull
// took the run past its deadline, and the diagnostics engine reported
// "[TIMEOUT_DURING_FAULT] test timed out while faults were active" —
// naming a cause that did not exist and pointing at retry loops and
// deadlocks that were not the problem.
func TestTimeoutWithNoFaultsIsNotBlamedOnFaults(t *testing.T) {
	tto := &TestTraceOutput{
		Name:        "test_baseline_boot",
		Result:      "fail",
		FailureType: "timeout",
		// No faults at all.
	}
	tr := &TestResult{Name: "test_baseline_boot", Result: "fail"}

	diags := buildDiagnostics(tto, tr)

	if hasCode(diags, "TIMEOUT_DURING_FAULT") {
		t.Errorf("a fault-free run was diagnosed as TIMEOUT_DURING_FAULT; got %v", diagCodes(diags))
	}
	if !hasCode(diags, "TIMEOUT_NO_FAULTS") {
		t.Errorf("expected TIMEOUT_NO_FAULTS, got %v", diagCodes(diags))
	}

	// The suggestion has to send the reader somewhere useful. For a
	// fault-free timeout that is service startup, not retry loops.
	for _, d := range diags {
		if d.Code != "TIMEOUT_NO_FAULTS" {
			continue
		}
		if d.Suggestion == "" {
			t.Error("TIMEOUT_NO_FAULTS carries no suggestion")
		}
	}
}

// TestTimeoutWithFiredFaultKeepsTheFaultDiagnosis asserts the original,
// correct behaviour is intact when a fault really did fire.
func TestTimeoutWithFiredFaultKeepsTheFaultDiagnosis(t *testing.T) {
	tto := &TestTraceOutput{
		Name:        "test_db_outage",
		Result:      "fail",
		FailureType: "timeout",
		Faults:      []FaultInfo{{Service: "db", Hits: 12}},
	}
	tr := &TestResult{Name: "test_db_outage", Result: "fail"}

	diags := buildDiagnostics(tto, tr)

	if !hasCode(diags, "TIMEOUT_DURING_FAULT") {
		t.Errorf("expected TIMEOUT_DURING_FAULT, got %v", diagCodes(diags))
	}
	for _, d := range diags {
		if d.Code == "TIMEOUT_DURING_FAULT" && d.Service != "db" {
			t.Errorf("diagnostic named service %q, want \"db\"", d.Service)
		}
	}
}

// TestTimeoutWithDeclaredButUnfiredFaultSaysSo covers the middle case.
// Faults existed but none fired, so the timeout cannot be attributed to
// them — and claiming otherwise is the same error in a subtler form.
func TestTimeoutWithDeclaredButUnfiredFaultSaysSo(t *testing.T) {
	tto := &TestTraceOutput{
		Name:        "test_db_outage",
		Result:      "fail",
		FailureType: "timeout",
		Faults:      []FaultInfo{{Service: "db", Hits: 0}},
	}
	tr := &TestResult{Name: "test_db_outage", Result: "fail"}

	diags := buildDiagnostics(tto, tr)

	if hasCode(diags, "TIMEOUT_DURING_FAULT") {
		t.Errorf("a timeout with no fault hits was blamed on the fault; got %v", diagCodes(diags))
	}
	if !hasCode(diags, "TIMEOUT_NO_FAULT_FIRED") {
		t.Errorf("expected TIMEOUT_NO_FAULT_FIRED, got %v", diagCodes(diags))
	}
}
