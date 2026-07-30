package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/faultbox/Faultbox/internal/check"
)

// `faultbox check` — the CLI wrapper around internal/check (RFC-052 Gap 1).
//
// The analysis lives in internal/check so the MCP tool runs exactly the same
// code. A check that behaved differently through MCP than through the CLI would
// be worse than no check, because the agent's model of the tool would be wrong.

func checkCmd(args []string) int {
	format := "text"
	maxInstances := -1
	var specFile string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			printCheckUsage(os.Stderr)
			return 0
		case args[i] == "--format" && i+1 < len(args):
			i++
			format = args[i]
		case strings.HasPrefix(args[i], "--format="):
			format = strings.TrimPrefix(args[i], "--format=")
		case args[i] == "--max-instances" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: --max-instances needs a number, got %q\n", args[i])
				return 1
			}
			maxInstances = n
		case strings.HasPrefix(args[i], "--max-instances="):
			n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--max-instances="))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: --max-instances needs a number")
				return 1
			}
			maxInstances = n
		case strings.HasPrefix(args[i], "-"):
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			printCheckUsage(os.Stderr)
			return 1
		default:
			specFile = args[i]
		}
	}

	if specFile == "" {
		fmt.Fprintln(os.Stderr, "error: no spec file given")
		printCheckUsage(os.Stderr)
		return 1
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(os.Stderr, "error: --format=%q not recognized (valid: text, json)\n", format)
		return 1
	}

	res := check.Run(specFile, maxInstances)

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		printCheckText(os.Stdout, res)
	}

	// Warnings do not fail the command. An agent branches on the JSON and CI
	// gates on whatever it chooses; forcing a non-zero exit for a warning would
	// make the default unusable for specs that legitimately carry one.
	for _, f := range res.Findings {
		if f.Level == "error" {
			return 2
		}
	}
	return 0
}

func printCheckText(w io.Writer, res *check.Result) {
	if len(res.Findings) == 0 {
		fmt.Fprintf(w, "ok  %s — %d test(s), %d plan instance(s)\n",
			res.Spec, len(res.Tests), res.PlanInstances)
		return
	}
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s: [%s] %s\n", strings.ToUpper(f.Level), f.Code, f.Message)
		if f.Suggestion != "" {
			fmt.Fprintf(w, "  %s\n", f.Suggestion)
		}
	}
	if res.OK {
		fmt.Fprintf(w, "\nok (warnings only) — %d test(s), %d plan instance(s)\n",
			len(res.Tests), res.PlanInstances)
	}
}

func printCheckUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: faultbox check <spec.star> [flags]

Validate a spec without running it. Launches no processes, pulls no images,
and does not need Docker.

Reports spec-load errors with machine-readable codes, the discovered tests, and
the plan's instance count.

Flags:
  --format=text|json     Output format (default text).
  --max-instances N      Report an error if the plan exceeds N instances.

Exit codes:
  0  no findings, or warnings only
  1  usage error
  2  one or more error-level findings

Note: whether a suite actually proves anything (NO_POSITIVE_CONTROL,
TEST_NO_ASSERTIONS) needs a run - see 'faultbox test'.
`)
}
