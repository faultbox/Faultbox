package plan

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/faultbox/Faultbox/internal/generate"
	"github.com/faultbox/Faultbox/internal/star"
)

// PlanEdge is one dependency edge in the topology graph, annotated with
// the test names that inject a fault on its target. FaultTests is empty
// for uncovered edges; the slice's length is the canonical coverage
// signal (RFC-042 §5.3).
type PlanEdge struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Protocol   string   `json:"protocol,omitempty"`
	Via        string   `json:"via,omitempty"` // "depends_on" | "env"
	FaultTests []string `json:"fault_tests,omitempty"`
}

// PlanCoverage summarizes test coverage against the topology. Populated
// by WithCoverage and consumed by the CLI's --coverage flag and the
// report's Plan tab.
type PlanCoverage struct {
	Edges          []PlanEdge `json:"edges"`
	UncoveredEdges int        `json:"uncovered_edges"`
	// CoveredProtocols and UncoveredProtocols feed §5.3's "Protocol
	// coverage" signal. Surface in JSON only when non-empty so the
	// minimum bundle stays small.
	CoveredProtocols   []string `json:"covered_protocols,omitempty"`
	UncoveredProtocols []string `json:"uncovered_protocols,omitempty"`
}

// WithCoverage mutates pt in place to populate Topology edges and the
// Coverage summary. Caller drives this when the user passes --coverage
// (or whenever the bundle wants the data along for the ride). Cheap —
// it walks rt.Services() and the plan-tree once.
//
// Coverage rule: an edge A→B is covered iff some PlanTest's
// fault_assumption(target=B) appears in the plan. Today we infer the
// target by walking FaultAssumptions and matching by service name; the
// joinTestsToTarget pass produces a service → []test_name map that
// PlanEdge.FaultTests reads.
func WithCoverage(pt *PlanTree, rt *star.Runtime) error {
	if pt == nil || rt == nil {
		return nil
	}
	analysis, err := generate.Analyze(rt)
	if err != nil {
		return fmt.Errorf("plan coverage: analyze: %w", err)
	}

	testsByTarget := joinTestsToTarget(rt)

	edges := make([]PlanEdge, 0, len(analysis.Edges))
	uncovered := 0
	for _, e := range analysis.Edges {
		pe := PlanEdge{
			From:       e.From,
			To:         e.To,
			Protocol:   e.Protocol,
			Via:        e.Via,
			FaultTests: append([]string(nil), testsByTarget[e.To]...),
		}
		if len(pe.FaultTests) == 0 {
			uncovered++
		}
		sort.Strings(pe.FaultTests)
		edges = append(edges, pe)
	}

	// Protocol coverage: union of protocols on edges with at least one
	// fault test (covered) vs. those without (uncovered). A protocol
	// that is covered by some edge but not by another lands in covered
	// only — the "uncovered" signal is per-protocol, not per-edge.
	covered := map[string]bool{}
	all := map[string]bool{}
	for _, e := range edges {
		if e.Protocol == "" {
			continue
		}
		all[e.Protocol] = true
		if len(e.FaultTests) > 0 {
			covered[e.Protocol] = true
		}
	}
	var coveredP, uncoveredP []string
	for p := range all {
		if covered[p] {
			coveredP = append(coveredP, p)
		} else {
			uncoveredP = append(uncoveredP, p)
		}
	}
	sort.Strings(coveredP)
	sort.Strings(uncoveredP)

	pt.Coverage = &PlanCoverage{
		Edges:              edges,
		UncoveredEdges:     uncovered,
		CoveredProtocols:   coveredP,
		UncoveredProtocols: uncoveredP,
	}
	return nil
}

// joinTestsToTarget produces a target-service → []test_name map by
// walking rt.FaultScenarios and rt.FaultAssumptions and recording which
// service each fault_assumption was bound to (the `target=` kwarg).
//
// fault_scenario(faults=fa) → test_<scenario>: faulting fa's targets.
// fault_matrix cells are attributed to the COLLAPSED PlanTest name
// "test_matrix_<scenarioName>" (matching what buildTests emits in the
// tree), not the per-cell name — so coverage links point at the same
// rows users see in the plan tree.
func joinTestsToTarget(rt *star.Runtime) map[string][]string {
	out := make(map[string][]string)
	for _, fs := range rt.FaultScenarios() {
		if fs == nil {
			continue
		}
		// Matrix cells collapse to one PlanTest keyed by scenario; use
		// the same name here so the link targets are valid (review B4).
		var testName string
		if fs.Matrix != nil {
			testName = "test_matrix_" + fs.Matrix.ScenarioName
		} else {
			testName = "test_" + fs.Name
		}
		for _, fa := range fs.Faults {
			if fa == nil {
				continue
			}
			// A FaultAssumption is N rules across services / interfaces;
			// any service mentioned counts as "faulted by this test."
			seen := map[string]struct{}{}
			for _, r := range fa.Rules {
				if r.Target == nil {
					continue
				}
				if _, dup := seen[r.Target.Name]; dup {
					continue
				}
				seen[r.Target.Name] = struct{}{}
				out[r.Target.Name] = appendUnique(out[r.Target.Name], testName)
			}
			for _, pr := range fa.ProxyRules {
				if pr.Target == nil || pr.Target.Service == nil {
					continue
				}
				name := pr.Target.Service.Name
				if _, dup := seen[name]; dup {
					continue
				}
				seen[name] = struct{}{}
				out[name] = appendUnique(out[name], testName)
			}
		}
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// WriteCoverageText renders the coverage table in §5.1's format. Used
// by `faultbox plan --coverage`. Returns the number of uncovered edges
// (for the CLI's exit code under --strict-coverage, which lands in PR 5
// follow-up).
func WriteCoverageText(w io.Writer, pt *PlanTree) (uncovered int) {
	if pt == nil || pt.Coverage == nil {
		fmt.Fprintln(w, "Coverage: no topology data available")
		return 0
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Coverage:\n")
	fmt.Fprintf(w, "- %d service%s declared\n", len(pt.Topology.Services), planPluralS(len(pt.Topology.Services)))
	fmt.Fprintf(w, "- %d dependency edge%s\n", len(pt.Coverage.Edges), planPluralS(len(pt.Coverage.Edges)))
	for _, e := range pt.Coverage.Edges {
		mark := "⚠"
		desc := "no fault test"
		if len(e.FaultTests) > 0 {
			mark = "✓"
			desc = "faulted in: " + strings.Join(e.FaultTests, ", ")
		}
		fmt.Fprintf(w, "  %s %s → %s  (%s)\n", mark, e.From, e.To, desc)
	}
	if pt.Coverage.UncoveredEdges > 0 {
		fmt.Fprintf(w, "\n%d edge%s without fault coverage — see `faultbox plan --suggest` for proposed tests.\n",
			pt.Coverage.UncoveredEdges, planPluralS(pt.Coverage.UncoveredEdges))
	}
	return pt.Coverage.UncoveredEdges
}

// planPluralS is the package-local pluralizer; the CLI has its own
// (cmd/faultbox/plan.go) to keep that file self-contained.
func planPluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
