package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/plan"
	"github.com/faultbox/Faultbox/internal/star"
)

// planCmd handles: faultbox plan <spec.star> [flags]
//
// rc1 ships the text format (RFC-042 §5.1). PR 3 wires --format=json
// and --format=dot. PR 5 adds --coverage / --suggest / --check-cost.
// rc2 wires the reserved flags (--strategy=llm and the interleavings
// dpor/sut-internal stubs) per §8.11.
func planCmd(args []string) int {
	var specFile string
	format := "text"
	var coverage, suggest, checkCost bool
	maxInstances := -1
	var strategy string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			printPlanUsage(os.Stderr)
			return 0
		case args[i] == "--format" && i+1 < len(args):
			i++
			format = args[i]
		case strings.HasPrefix(args[i], "--format="):
			format = strings.TrimPrefix(args[i], "--format=")
		case args[i] == "--coverage":
			coverage = true
		case args[i] == "--suggest":
			suggest = true
			coverage = true // suggestions imply coverage
		case args[i] == "--strategy" && i+1 < len(args):
			i++
			strategy = args[i]
		case strings.HasPrefix(args[i], "--strategy="):
			strategy = strings.TrimPrefix(args[i], "--strategy=")
		case args[i] == "--check-cost":
			checkCost = true
		case args[i] == "--max-instances" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: --max-instances must be an integer, got %q\n", args[i])
				return 1
			}
			maxInstances = n
		case strings.HasPrefix(args[i], "--max-instances="):
			n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--max-instances="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: --max-instances must be an integer\n")
				return 1
			}
			maxInstances = n
		case strings.HasSuffix(args[i], ".star"):
			specFile = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			return 1
		}
	}

	if specFile == "" {
		fmt.Fprintln(os.Stderr, "error: .star file required")
		printPlanUsage(os.Stderr)
		return 1
	}

	// --check-cost without --max-instances is a silent CI footgun
	// (the gate would always pass); fail loudly per review note N1.
	if checkCost && maxInstances < 0 {
		fmt.Fprintln(os.Stderr, "error: --check-cost requires --max-instances N (the budget to gate against)")
		return 1
	}

	// RFC-042 §8.11 — --strategy=llm is reserved; reject early so users
	// see the migration path now (v0.14.0 / RFC-043 lands the LLM path
	// via MCP).
	if strategy != "" && strategy != "rules" {
		if strategy == "llm" {
			fmt.Fprintln(os.Stderr, "error: --strategy=llm requires Faultbox v0.14.0 / RFC-043 MCP bundle ops; not available in this release")
			return 1
		}
		fmt.Fprintf(os.Stderr, "error: --strategy=%q not recognized (valid: rules; reserved: llm)\n", strategy)
		return 1
	}

	logger := logging.New(logging.Config{Level: slog.LevelWarn})
	rt := star.New(logger)
	if err := rt.LoadFile(specFile); err != nil {
		fmt.Fprintf(os.Stderr, "error loading %s: %v\n", specFile, err)
		return 1
	}

	pt := plan.Enumerate(rt)
	if coverage {
		if err := plan.WithCoverage(pt, rt); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}

	switch format {
	case "text", "":
		renderPlanText(os.Stdout, pt)
		if coverage {
			plan.WriteCoverageText(os.Stdout, pt)
		}
		if suggest {
			fmt.Fprintln(os.Stdout)
			plan.WriteSuggestions(os.Stdout, pt)
		}
	case "json":
		if err := plan.WriteJSON(os.Stdout, pt); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	case "dot":
		if err := plan.WriteDOT(os.Stdout, pt); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --format=%s (valid: text, json, dot)\n", format)
		return 1
	}

	// RFC-042 §8.6 — cost gate. Exit non-zero when instance count
	// exceeds the user's budget, so CI pre-commit hooks can fail loudly
	// before launching a multi-hour run.
	if checkCost && maxInstances >= 0 && pt.Totals.Instances > maxInstances {
		fmt.Fprintf(os.Stderr, "\ncost gate: plan has %d instances; --max-instances=%d exceeded\n",
			pt.Totals.Instances, maxInstances)
		return 2
	}
	return 0
}

func printPlanUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: faultbox plan <file.star> [flags]

Statically enumerate the plan tree for a spec — every test, every
fault_matrix cell, every fault_scenario — without launching any
service or executing any test. Useful for:

  • previewing the test count and structure before running CI
  • diagnosing fault_matrix that exploded into more rows than expected
  • feeding the plan tree to other tools (JSON for jq/LLMs, DOT for graphviz)

Flags:
  --format=text                Human-readable tree (default)
  --format=json                Structured JSON (RFC-042 §5.2)
  --format=dot                 Graphviz DOT (pipe into 'dot -Tsvg')
  --coverage                   Append coverage table: edges, fault tests, gaps
  --suggest [--strategy=rules] Print copy-pasteable stubs for uncovered edges.
                               --strategy=llm is reserved for v0.14.0 (RFC-043).
  --check-cost --max-instances N
                               Exit non-zero if plan has > N instances.
                               Useful as a pre-commit / CI cost gate.
  -h, --help                   Show this help`)
}

// renderPlanText writes the human-readable plan tree to w. The format
// follows RFC-042 §5.1: header (spec / seed / determinism), tests with
// instance counts and composition axes, and a totals line at the end.
func renderPlanText(w io.Writer, pt *plan.PlanTree) {
	if pt.SpecPath != "" {
		fmt.Fprintf(w, "Spec: %s\n", pt.SpecPath)
	}
	if pt.Seed != nil {
		fmt.Fprintf(w, "Seed: %d\n", *pt.Seed)
	} else {
		fmt.Fprintln(w, "Seed: (unseeded)")
	}
	fmt.Fprintf(w, "Determinism: %s", pt.Determinism.Level)
	if pt.Determinism.Strict {
		fmt.Fprint(w, " (strict)")
	}
	if !pt.Determinism.Explicit {
		fmt.Fprint(w, " — default; spec did not call determinism()")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	if len(pt.Tests) == 0 {
		fmt.Fprintln(w, "Plan tree: (no tests discovered)")
		return
	}

	fmt.Fprintf(w, "Plan tree:\n└── %d test%s\n", len(pt.Tests), planPluralS(len(pt.Tests)))
	for i, t := range pt.Tests {
		isLast := i == len(pt.Tests)-1
		renderPlanTest(w, t, isLast)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Total: %d plan instance%s\n", pt.Totals.Instances, planPluralS(pt.Totals.Instances))

	if len(pt.Topology.Services) > 0 {
		fmt.Fprintf(w, "Services: %d (", len(pt.Topology.Services))
		for i, s := range pt.Topology.Services {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprint(w, s.Name)
		}
		fmt.Fprintln(w, ")")
	}
}

// renderPlanTest emits one test entry with correct tree-drawing
// connectors. Each child line of the test (instances count,
// composition block, faults, expect, timeout) gets `├──` for all but
// the last, and `└──` for the last — so the rendering survives both
// a script tail/head and a human eye scan.
func renderPlanTest(w io.Writer, t plan.PlanTest, isLast bool) {
	branch := "    ├── "
	cont := "    │   "
	if isLast {
		branch = "    └── "
		cont = "        "
	}

	fmt.Fprintf(w, "%stest %q  [%s]\n", branch, t.Name, t.Kind)

	// Collect children of this test as (label, sublines) pairs so we
	// can stamp the connector character once at the end.
	type child struct {
		label string
		sub   []string
	}
	var children []child

	if t.Instances != 1 {
		children = append(children, child{label: fmt.Sprintf("%d instances", t.Instances)})
	}
	for _, comp := range t.Compositions {
		var sub []string
		for _, ax := range comp.Axes {
			sub = append(sub, fmt.Sprintf("%s: [%s]", ax.Name, strings.Join(ax.Values, ", ")))
		}
		children = append(children, child{label: string(comp.Kind), sub: sub})
	}
	if len(t.Faults) > 0 {
		children = append(children, child{label: fmt.Sprintf("faults: [%s]", strings.Join(t.Faults, ", "))})
	}
	switch t.Expect {
	case "":
		// no expect
	case "(mixed)":
		children = append(children, child{label: "expect: (mixed — per-cell overrides)"})
	default:
		children = append(children, child{label: "expect: " + t.Expect})
	}
	if t.Timeout != "" {
		children = append(children, child{label: "timeout: " + t.Timeout})
	}

	for i, c := range children {
		childBranch := "├── "
		childCont := "│   "
		if i == len(children)-1 {
			childBranch = "└── "
			childCont = "    "
		}
		fmt.Fprintf(w, "%s%s%s\n", cont, childBranch, c.label)
		for j, s := range c.sub {
			leaf := "├── "
			if j == len(c.sub)-1 {
				leaf = "└── "
			}
			fmt.Fprintf(w, "%s%s%s%s\n", cont, childCont, leaf, s)
		}
	}
}

func planPluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
