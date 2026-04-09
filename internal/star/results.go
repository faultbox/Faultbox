package star

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// socketRe matches socket:[12345], pipe:[12345], anon_inode:[eventfd] etc.
var socketRe = regexp.MustCompile(`^(socket|pipe|anon_inode):\[\d+\]$`)

// TraceOutput is the JSON structure written to --output.
type TraceOutput struct {
	Version    int               `json:"version"`
	StarFile   string            `json:"star_file,omitempty"`
	DurationMs int64             `json:"duration_ms"`
	Pass       int               `json:"pass"`
	Fail       int               `json:"fail"`
	Tests      []TestTraceOutput `json:"tests"`
}

// TestTraceOutput is the per-test section of the trace output.
type TestTraceOutput struct {
	Name           string                          `json:"name"`
	Result         string                          `json:"result"`
	Reason         string                          `json:"reason,omitempty"`
	FailureType    string                          `json:"failure_type,omitempty"`
	Seed           uint64                          `json:"seed"`
	DurationMs     int64                           `json:"duration_ms"`
	ReplayCmd      string                          `json:"replay_command,omitempty"`
	ErrorDetail    *ErrorDetail                    `json:"error_detail,omitempty"`
	Faults         []FaultInfo                     `json:"faults,omitempty"`
	SyscallSummary map[string]*SyscallSummaryEntry `json:"syscall_summary,omitempty"`
	Events         []Event                         `json:"events"`
}

// FaultInfo describes a fault rule that was active during a test.
type FaultInfo struct {
	Service string `json:"service"`
	Syscall string `json:"syscall"`
	Action  string `json:"action"`
	Errno   string `json:"errno,omitempty"`
	Hits    int    `json:"hits"`
	Label   string `json:"label,omitempty"`
}

// SyscallSummaryEntry is per-service syscall statistics.
type SyscallSummaryEntry struct {
	Total     int            `json:"total"`
	Faulted   int            `json:"faulted"`
	Breakdown map[string]int `json:"breakdown"`
}

// ErrorDetail provides structured error info for machine consumption.
type ErrorDetail struct {
	AssertionType string `json:"assertion_type,omitempty"`
	Message       string `json:"message,omitempty"`
}

// BuildTraceOutput constructs the full JSON output structure from suite results.
func BuildTraceOutput(starFile string, result *SuiteResult) TraceOutput {
	out := TraceOutput{
		Version:    2,
		StarFile:   starFile,
		DurationMs: result.DurationMs,
		Pass:       result.Pass,
		Fail:       result.Fail,
		Tests:      make([]TestTraceOutput, 0, len(result.Tests)),
	}
	for _, tr := range result.Tests {
		tto := TestTraceOutput{
			Name:        tr.Name,
			Result:      tr.Result,
			Reason:      tr.Reason,
			FailureType: classifyFailure(tr.Reason),
			Seed:        tr.Seed,
			DurationMs:  tr.DurationMs,
			Events:      tr.Events,
		}
		if tr.Result == "fail" {
			tto.ReplayCmd = fmt.Sprintf("faultbox test %s --test %s --seed %d",
				starFile, strings.TrimPrefix(tr.Name, "test_"), tr.Seed)
		}
		enrichTestOutput(&tto, &tr)
		out.Tests = append(out.Tests, tto)
	}
	return out
}

// WriteTraceResults writes the suite result with full event traces to a JSON file.
func WriteTraceResults(path, starFile string, result *SuiteResult) error {
	out := BuildTraceOutput(starFile, result)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trace results: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write trace results: %w", err)
	}
	return nil
}

// enrichTestOutput populates Faults, SyscallSummary, and ErrorDetail from events.
func enrichTestOutput(tto *TestTraceOutput, tr *TestResult) {
	// --- Error detail ---
	if tr.Result == "fail" && tr.Reason != "" {
		tto.ErrorDetail = &ErrorDetail{
			AssertionType: classifyFailure(tr.Reason),
			Message:       tr.Reason,
		}
	}

	// --- Syscall summary (per-service) ---
	summary := make(map[string]*SyscallSummaryEntry)
	for _, ev := range tr.Events {
		if ev.Type != "syscall" {
			continue
		}
		svc := ev.Service
		if summary[svc] == nil {
			summary[svc] = &SyscallSummaryEntry{Breakdown: make(map[string]int)}
		}
		s := summary[svc]
		s.Total++
		sc := ev.Fields["syscall"]
		s.Breakdown[sc]++
		decision := ev.Fields["decision"]
		if decision != "allow" && decision != "allow (system path)" && decision != "" {
			s.Faulted++
		}
	}
	if len(summary) > 0 {
		tto.SyscallSummary = summary
	}

	// --- Faults (from fault_applied/fault_removed events) ---
	type faultScope struct {
		service string
		syscall string
		details string
		seq     int64
	}
	var scopes []faultScope
	for _, ev := range tr.Events {
		if ev.Type != "fault_applied" {
			continue
		}
		for k, v := range ev.Fields {
			scopes = append(scopes, faultScope{
				service: ev.Service,
				syscall: k,
				details: v,
				seq:     ev.Seq,
			})
		}
	}

	for _, scope := range scopes {
		fi := FaultInfo{
			Service: scope.service,
			Syscall: scope.syscall,
		}

		// Parse action and errno from details like "deny(EIO) → filter:[...]"
		d := scope.details
		if strings.HasPrefix(d, "deny(") {
			fi.Action = "deny"
			if idx := strings.Index(d, ")"); idx > 5 {
				fi.Errno = d[5:idx]
			}
		} else if strings.HasPrefix(d, "delay(") {
			fi.Action = "delay"
			if idx := strings.Index(d, ")"); idx > 6 {
				fi.Errno = d[6:idx] // duration, reuse field
			}
		} else if strings.HasPrefix(d, "trace") {
			fi.Action = "trace"
		}

		// Extract label
		if idx := strings.Index(d, `label="`); idx >= 0 {
			rest := d[idx+7:]
			if end := strings.Index(rest, `"`); end >= 0 {
				fi.Label = rest[:end]
			}
		}

		// Count hits: syscall events on this service between fault_applied and fault_removed
		endSeq := int64(1<<62 - 1)
		for _, ev := range tr.Events {
			if ev.Type == "fault_removed" && ev.Service == scope.service && ev.Seq > scope.seq {
				endSeq = ev.Seq
				break
			}
		}
		for _, ev := range tr.Events {
			if ev.Type == "syscall" && ev.Service == scope.service &&
				ev.Seq > scope.seq && ev.Seq < endSeq {
				decision := ev.Fields["decision"]
				if decision != "allow" && decision != "allow (system path)" && decision != "" {
					fi.Hits++
				}
			}
		}

		tto.Faults = append(tto.Faults, fi)
	}
}

// classifyFailure categorizes a failure reason for machine consumption.
func classifyFailure(reason string) string {
	if reason == "" {
		return ""
	}
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "assert_eq") || strings.Contains(r, "assert_true") ||
		strings.Contains(r, "assert_eventually") || strings.Contains(r, "assert_never"):
		return "assertion"
	case strings.Contains(r, "timed out") || strings.Contains(r, "timeout"):
		return "timeout"
	case strings.Contains(r, "failed to start"):
		return "service_start"
	default:
		return "error"
	}
}

// normalizePath strips non-deterministic parts from paths.
// socket:[12345] → socket, pipe:[67890] → pipe.
func normalizePath(path string) string {
	if socketRe.MatchString(path) {
		return path[:strings.Index(path, ":")]
	}
	return path
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
				svc = "test"
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

				// Normalize non-deterministic paths:
				// socket:[12345] → socket (inode numbers change between runs)
				// pipe:[12345] → pipe
				path = normalizePath(path)

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

// ANSI color codes for terminal output.
const (
	colorRed   = "\033[31m"
	colorGreen = "\033[32m"
	colorYellow = "\033[33m"
	colorReset = "\033[0m"
)

// DiffTraces compares two normalized trace strings and returns a human-readable diff.
// Returns empty string if traces are identical. Output uses ANSI colors
// with context lines around each difference (like git diff).
func DiffTraces(a, b string) string {
	linesA := strings.Split(strings.TrimSpace(a), "\n")
	linesB := strings.Split(strings.TrimSpace(b), "\n")

	if len(linesA) == 0 && len(linesB) == 0 {
		return ""
	}

	maxLen := len(linesA)
	if len(linesB) > maxLen {
		maxLen = len(linesB)
	}

	// Find which lines differ.
	type diffLine struct {
		lineNum int
		a, b    string
	}
	var diffs []diffLine
	for i := 0; i < maxLen; i++ {
		var la, lb string
		if i < len(linesA) {
			la = linesA[i]
		}
		if i < len(linesB) {
			lb = linesB[i]
		}
		if la != lb {
			diffs = append(diffs, diffLine{i, la, lb})
		}
	}

	if len(diffs) == 0 {
		return ""
	}

	var sb strings.Builder
	header := fmt.Sprintf("%straces differ (%d vs %d lines):%s\n", colorYellow, len(linesA), len(linesB), colorReset)
	sb.WriteString(header)

	lastShown := -1
	for _, d := range diffs {
		// Show 1 context line before the diff (if not already shown).
		ctx := d.lineNum - 1
		if ctx >= 0 && ctx > lastShown && ctx < len(linesA) {
			fmt.Fprintf(&sb, "  line %d:  %s  (same in both)\n", ctx+1, linesA[ctx])
		}
		fmt.Fprintf(&sb, "  line %d:\n", d.lineNum+1)
		fmt.Fprintf(&sb, "    %s- %s%s\n", colorRed, d.a, colorReset)
		fmt.Fprintf(&sb, "    %s+ %s%s\n", colorGreen, d.b, colorReset)
		lastShown = d.lineNum
	}

	return sb.String()
}

// WriteShiVizTrace writes a ShiViz-compatible trace file for a test result.
func WriteShiVizTrace(path string, result *SuiteResult) error {
	// Build a combined EventLog from all test events.
	// Vector clocks must be globally monotonic, so we offset each test's
	// clocks by the maximum seen so far per host.
	log := NewEventLog()
	maxClock := make(map[string]int64) // host → max clock value seen

	for _, tr := range result.Tests {
		// Calculate offset: shift this test's clocks so they start after
		// the previous test's maximum.
		offset := make(map[string]int64)
		for host, max := range maxClock {
			offset[host] = max
		}

		for _, ev := range tr.Events {
			// Apply offset to vector clock.
			if ev.VectorClock != nil {
				adjusted := make(map[string]int64, len(ev.VectorClock))
				for host, val := range ev.VectorClock {
					adjusted[host] = val + offset[host]
				}
				ev.VectorClock = adjusted
			}

			log.mu.Lock()
			log.seq++
			ev.Seq = log.seq
			log.events = append(log.events, ev)
			log.mu.Unlock()

			// Track max clock per host.
			for host, val := range ev.VectorClock {
				if val > maxClock[host] {
					maxClock[host] = val
				}
			}
		}

		// Inject a violation marker for failed tests.
		if tr.Result == "fail" {
			// Place the violation on "test" host with a clock that
			// follows the last event, merging all known service clocks.
			vc := make(map[string]int64)
			for host, val := range maxClock {
				vc[host] = val
			}
			// Advance "test" clock for this event.
			vc["test"] = vc["test"] + 1
			maxClock["test"] = vc["test"]

			reason := tr.Reason
			if reason == "" {
				reason = "test failed"
			}

			log.mu.Lock()
			log.seq++
			log.events = append(log.events, Event{
				Seq:         log.seq,
				Type:        "violation",
				EventType:   "violation",
				Service:     "test",
				VectorClock: vc,
				Fields: map[string]string{
					"test":   tr.Name,
					"reason": reason,
				},
			})
			log.mu.Unlock()
		}
	}

	content := log.FormatShiViz()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write shiviz trace: %w", err)
	}
	return nil
}
