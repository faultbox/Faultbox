package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
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

	logger := logging.New(logging.Config{Level: slog.LevelWarn})
	rt := star.New(logger)
	if err := rt.LoadFile(specFile); err != nil {
		fmt.Fprintf(os.Stderr, "error loading %s: %v\n", specFile, err)
		return 1
	}

	pt := plan.Enumerate(rt)

	switch format {
	case "text", "":
		renderPlanText(os.Stdout, pt)
	case "json", "dot":
		// Reserved here so the flag surface is stable from rc1; PR 3
		// implements both. Returning early with a clear message keeps
		// CI integrations from silently getting wrong output.
		fmt.Fprintf(os.Stderr, "error: --format=%s lands in v0.13.0-rc1 PR 3; only --format=text is available today\n", format)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --format=%s (valid: text, json, dot)\n", format)
		return 1
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
  • feeding the plan tree to other tools (JSON/DOT output — PR 3)

Flags:
  --format=text          Human-readable tree (default)
  --format=json          Structured JSON (RFC-042 §5.2) — coming soon
  --format=dot           Graphviz DOT — coming soon
  -h, --help             Show this help`)
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

func renderPlanTest(w io.Writer, t plan.PlanTest, isLast bool) {
	branch := "    ├── "
	cont := "    │   "
	if isLast {
		branch = "    └── "
		cont = "        "
	}

	fmt.Fprintf(w, "%stest %q  [%s]\n", branch, t.Name, t.Kind)
	if t.Instances != 1 {
		fmt.Fprintf(w, "%s└── %d instances\n", cont, t.Instances)
	}
	for _, comp := range t.Compositions {
		fmt.Fprintf(w, "%s    └── %s\n", cont, comp.Kind)
		for _, ax := range comp.Axes {
			fmt.Fprintf(w, "%s        ├── %s: [%s]\n", cont, ax.Name, strings.Join(ax.Values, ", "))
		}
	}
	if len(t.Faults) > 0 {
		fmt.Fprintf(w, "%s    └── faults: [%s]\n", cont, strings.Join(t.Faults, ", "))
	}
	if t.Expect != "" && t.Expect != "(mixed)" {
		fmt.Fprintf(w, "%s    └── expect: %s\n", cont, t.Expect)
	} else if t.Expect == "(mixed)" {
		fmt.Fprintf(w, "%s    └── expect: (mixed — per-cell overrides)\n", cont)
	}
	if t.Timeout != "" {
		fmt.Fprintf(w, "%s    └── timeout: %s\n", cont, t.Timeout)
	}
}

func planPluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
