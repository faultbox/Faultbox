package plan

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteSuggestions emits copy-pasteable Starlark stubs for every
// uncovered edge in the plan's coverage data. RFC-042 §5.4 (rule-based,
// v0.13.0). The stub form is intentionally minimal — fault_assumption +
// fault_scenario referencing already-declared service variables — so
// users can drop it into the spec without rewriting.
//
// Returns the number of stubs emitted so the CLI can warn when nothing
// is suggested ("everything is covered" or "no topology data").
//
// The LLM-driven variant lives behind --strategy=llm and is reserved
// for v0.14.0 / RFC-043; the CLI rejects that flag today with a clear
// "available in a future release" error.
func WriteSuggestions(w io.Writer, pt *PlanTree) int {
	if pt == nil || pt.Coverage == nil {
		fmt.Fprintln(w, "# No coverage data — run with --coverage to enable suggestions.")
		return 0
	}
	var uncovered []PlanEdge
	for _, e := range pt.Coverage.Edges {
		if len(e.FaultTests) == 0 {
			uncovered = append(uncovered, e)
		}
	}
	if len(uncovered) == 0 {
		fmt.Fprintln(w, "# All dependency edges have at least one fault test. Nothing to suggest.")
		return 0
	}
	sort.Slice(uncovered, func(i, j int) bool {
		if uncovered[i].From == uncovered[j].From {
			return uncovered[i].To < uncovered[j].To
		}
		return uncovered[i].From < uncovered[j].From
	})

	fmt.Fprintln(w, "# Rule-based suggestions for uncovered dependency edges.")
	fmt.Fprintln(w, "# Paste the snippets you want into your .star file and adjust to taste.")
	fmt.Fprintln(w, "# LLM-driven suggestions ship in v0.14.0 (RFC-043, --strategy=llm).")
	fmt.Fprintln(w)

	for _, e := range uncovered {
		writeEdgeSuggestion(w, e)
	}
	return len(uncovered)
}

func writeEdgeSuggestion(w io.Writer, e PlanEdge) {
	faultName := fmt.Sprintf("%s_unavailable", e.To)
	scenarioName := fmt.Sprintf("scenario_%s_calls_%s", e.From, e.To)
	testName := fmt.Sprintf("%s_when_%s_down", e.From, e.To)

	// Pick a plausible syscall fault per protocol. The list is
	// deliberately conservative — every protocol gets connect=deny
	// because every edge involves at least one connect on first call.
	syscall := pickSyscallForProtocol(e.Protocol)

	fmt.Fprintf(w, "# Uncovered edge: %s → %s", e.From, e.To)
	if e.Protocol != "" {
		fmt.Fprintf(w, " (%s)", e.Protocol)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s = fault_assumption(%q,\n", faultName, faultName)
	fmt.Fprintf(w, "    target = %s,\n", e.To)
	fmt.Fprintf(w, "    %s = deny(%q),\n", syscall, "ECONNREFUSED")
	fmt.Fprintln(w, ")")
	fmt.Fprintf(w, "def %s():\n    # TODO: call %s functionality that exercises %s.\n    pass\n",
		scenarioName, e.From, e.To)
	fmt.Fprintf(w, "fault_scenario(%q,\n", testName)
	fmt.Fprintf(w, "    scenario = %s,\n", scenarioName)
	fmt.Fprintf(w, "    faults   = %s,\n", faultName)
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w)
}

// pickSyscallForProtocol maps a protocol name to the syscall most
// likely to surface the dependency failure. Conservative — the LLM
// strategy in v0.14.0 will pick smarter targets per call pattern.
func pickSyscallForProtocol(protocol string) string {
	switch strings.ToLower(protocol) {
	case "tcp", "http", "http2", "grpc", "redis", "postgres", "mysql",
		"kafka", "nats", "mongodb", "cassandra", "clickhouse":
		return "connect"
	case "udp":
		return "sendto"
	default:
		return "connect"
	}
}
