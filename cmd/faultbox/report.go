package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/faultbox/Faultbox/internal/bundle"
	"github.com/faultbox/Faultbox/internal/report"
)

// reportCmd handles `faultbox report <bundle.fb>`:
//
//	faultbox report run.fb                 # writes report.html next to the bundle
//	faultbox report run.fb --output r.html # explicit output path
//	faultbox report run.fb -o -            # write to stdout
//
// The report is a single self-contained HTML file: the report reads
// exactly one `.fb` bundle as input (RFC-025), inlines manifest, env
// and trace JSONs into a <script> tag, and renders everything client
// side. No network, no build step, no server — users can email it,
// Slack it, commit it to git, or publish it as a CI artifact.
//
// v0.11.0 ships Phase 1: header, hero stats, fault matrix, attention
// list, reproducibility panel, tests-table fallback. Phase 2 adds
// drill-down modals, observed coverage, and the swim-lane trace
// viewer per RFC-029.
func reportCmd(args []string) int {
	var bundlePath, outPath string
	for len(args) > 0 {
		switch {
		case strings.HasPrefix(args[0], "--output="):
			outPath = strings.TrimPrefix(args[0], "--output=")
		case args[0] == "--output" && len(args) > 1:
			outPath = args[1]
			args = args[1:]
		case args[0] == "-o" && len(args) > 1:
			outPath = args[1]
			args = args[1:]
		case args[0] == "-h", args[0] == "--help":
			printReportUsage()
			return 0
		case strings.HasPrefix(args[0], "-"):
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[0])
			printReportUsage()
			return 1
		case bundlePath == "":
			bundlePath = args[0]
		default:
			fmt.Fprintf(os.Stderr, "unexpected argument: %s\n", args[0])
			printReportUsage()
			return 1
		}
		args = args[1:]
	}

	if bundlePath == "" {
		fmt.Fprintln(os.Stderr, "faultbox report: bundle path required")
		printReportUsage()
		return 1
	}

	r, err := bundle.Open(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Soft-gate version warning (same policy as `inspect`: never
	// refuse on a read-only path).
	printVersionBannerIfDrift(os.Stderr, r, faultboxVersion())

	if outPath == "" {
		outPath = defaultReportPath(bundlePath)
	}

	if outPath == "-" {
		if err := report.Build(os.Stdout, r, faultboxVersion()); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}

	if err := report.BuildToFile(outPath, r, faultboxVersion()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	return 0
}

// defaultReportPath picks `report.html` alongside the bundle. Fixed
// name (not `<bundle>.report.html`) because the common flow is "open
// the report for the most recent run" — one canonical filename keeps
// muscle memory predictable. Users who want a kept archive pass
// `--output`.
func defaultReportPath(bundlePath string) string {
	dir := filepath.Dir(bundlePath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "report.html")
}

func printReportUsage() {
	const usage = `faultbox report — build a self-contained HTML report from a .fb bundle

USAGE
  faultbox report <bundle.fb>                     # writes report.html next to the bundle
  faultbox report <bundle.fb> --output <path>     # custom output path
  faultbox report <bundle.fb> -o -                # write to stdout

The output is a single HTML file with all CSS, JS and bundle data
inlined. It opens in any browser with no network access.

EXAMPLES
  faultbox report run-2026-04-22-42.fb
  faultbox report run.fb --output report.html
  faultbox report run.fb -o - | less
`
	fmt.Fprint(os.Stderr, usage)
}
