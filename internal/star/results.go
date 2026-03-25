package star

import (
	"encoding/json"
	"fmt"
	"os"
)

// TraceOutput is the JSON structure written to --output.
type TraceOutput struct {
	DurationMs int64             `json:"duration_ms"`
	Pass       int               `json:"pass"`
	Fail       int               `json:"fail"`
	Tests      []TestTraceOutput `json:"tests"`
}

// TestTraceOutput is the per-test section of the trace output.
type TestTraceOutput struct {
	Name       string  `json:"name"`
	Result     string  `json:"result"`
	Reason     string  `json:"reason,omitempty"`
	DurationMs int64   `json:"duration_ms"`
	Events     []Event `json:"events"`
}

// WriteTraceResults writes the suite result with full event traces to a JSON file.
func WriteTraceResults(path string, result *SuiteResult) error {
	out := TraceOutput{
		DurationMs: result.DurationMs,
		Pass:       result.Pass,
		Fail:       result.Fail,
		Tests:      make([]TestTraceOutput, 0, len(result.Tests)),
	}
	for _, tr := range result.Tests {
		out.Tests = append(out.Tests, TestTraceOutput{
			Name:       tr.Name,
			Result:     tr.Result,
			Reason:     tr.Reason,
			DurationMs: tr.DurationMs,
			Events:     tr.Events,
		})
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trace results: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write trace results: %w", err)
	}
	return nil
}

// WriteShiVizTrace writes a ShiViz-compatible trace file for a test result.
func WriteShiVizTrace(path string, result *SuiteResult) error {
	// Build a combined EventLog from all test events.
	log := NewEventLog()
	for _, tr := range result.Tests {
		for _, ev := range tr.Events {
			// Re-emit using raw fields to preserve vector clocks.
			log.mu.Lock()
			log.seq++
			ev.Seq = log.seq
			log.events = append(log.events, ev)
			log.mu.Unlock()
		}
	}

	content := log.FormatShiViz()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write shiviz trace: %w", err)
	}
	return nil
}
