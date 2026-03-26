package star

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
	Seed       uint64  `json:"seed"`
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
			Seed:       tr.Seed,
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

// NormalizeTrace produces a deterministic fingerprint of a test's syscall trace.
// Groups events by service to eliminate cross-service interleaving nondeterminism
// (caused by Go scheduler ordering). Each service's own syscall sequence is
// deterministic because it follows the same code path on the same input.
func NormalizeTrace(result *SuiteResult) string {
	var sb strings.Builder
	for _, tr := range result.Tests {
		fmt.Fprintf(&sb, "=== %s ===\n", tr.Name)

		// Collect events per service (preserving order within each service).
		perService := make(map[string][]string)
		var serviceOrder []string
		seen := make(map[string]bool)

		for _, ev := range tr.Events {
			var line string
			svc := ev.Service
			if svc == "" {
				svc = "faultbox"
			}

			switch ev.Type {
			case "syscall":
				decision := ev.Fields["decision"]
				syscall := ev.Fields["syscall"]
				path := ev.Fields["path"]

				// Skip generic allowed syscalls without paths — these are
				// Go runtime noise whose ordering is nondeterministic.
				if (decision == "allow" || decision == "allow (system path)") && path == "" {
					continue
				}

				line = fmt.Sprintf("%s %s", syscall, decision)
				if path != "" {
					line += " " + path
				}
			case "service_started", "service_ready", "fault_applied", "fault_removed":
				line = ev.Type
			case "step_send", "step_recv":
				target := ev.Fields["target"]
				line = fmt.Sprintf("%s %s", ev.Type, target)
			default:
				continue
			}

			if !seen[svc] {
				serviceOrder = append(serviceOrder, svc)
				seen[svc] = true
			}
			perService[svc] = append(perService[svc], line)
		}

		// Output per-service traces in deterministic order.
		for _, svc := range serviceOrder {
			fmt.Fprintf(&sb, "--- %s ---\n", svc)
			for _, line := range perService[svc] {
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// WriteNormalizedTrace writes the normalized (deterministic) trace to a file.
func WriteNormalizedTrace(path string, result *SuiteResult) error {
	content := NormalizeTrace(result)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write normalized trace: %w", err)
	}
	return nil
}

// DiffTraces compares two normalized trace strings and returns a human-readable diff.
// Returns empty string if traces are identical.
func DiffTraces(a, b string) string {
	linesA := strings.Split(strings.TrimSpace(a), "\n")
	linesB := strings.Split(strings.TrimSpace(b), "\n")

	if len(linesA) == 0 && len(linesB) == 0 {
		return ""
	}

	var sb strings.Builder
	maxLen := len(linesA)
	if len(linesB) > maxLen {
		maxLen = len(linesB)
	}

	identical := true
	for i := 0; i < maxLen; i++ {
		var la, lb string
		if i < len(linesA) {
			la = linesA[i]
		}
		if i < len(linesB) {
			lb = linesB[i]
		}
		if la != lb {
			identical = false
			fmt.Fprintf(&sb, "  line %d:\n    run1: %s\n    run2: %s\n", i+1, la, lb)
		}
	}

	if identical {
		return ""
	}

	header := fmt.Sprintf("traces differ (%d vs %d lines):\n", len(linesA), len(linesB))
	return header + sb.String()
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
