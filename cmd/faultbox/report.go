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
//	faultbox report run-<ts>-<seed>.fb     # writes report_<ts>-<seed>.html alongside
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
	var opts report.Options
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
		case args[0] == "--summary":
			opts.Summary = true
		case args[0] == "--full-events":
			opts.FullEvents = true
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
		if err := report.BuildWithOptions(os.Stdout, r, faultboxVersion(), opts); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}

	if err := report.BuildToFileWithOptions(outPath, r, faultboxVersion(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	return 0
}

// defaultReportPath derives `report_<stem>.html` alongside the bundle,
// where <stem> is the bundle filename with its extension and any
// `run-` / `run_` prefix removed:
//
//	run-2026-07-30T10-50-37-1.fb  ->  report_2026-07-30T10-50-37-1.html
//	nightly.fb                    ->  report_nightly.html
//
// Faultbox names every bundle `run-<ts>-<seed>.fb` (bundle.writer), so
// stripping that prefix keeps the useful part — the timestamp and seed
// — without the redundant "run" once "report" is already in the name.
//
// This used to be the fixed name `report.html`, on the reasoning that
// one canonical filename is predictable. It is, but it also means
// reporting a second bundle silently overwrites the first, which is
// the wrong default when the two runs are what you want to compare.
// Users who want a fixed name pass `--output`.
func defaultReportPath(bundlePath string) string {
	dir := filepath.Dir(bundlePath)
	if dir == "" {
		dir = "."
	}

	base := filepath.Base(bundlePath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for _, prefix := range []string{"run-", "run_"} {
		if strings.HasPrefix(stem, prefix) {
			stem = strings.TrimPrefix(stem, prefix)
			break
		}
	}

	// A bundle called exactly `run-.fb` (or `.fb`, or `run.fb`) leaves
	// nothing to qualify the name with — fall back to the bare form
	// rather than emitting `report_.html`.
	if stem == "" || stem == "run" {
		return filepath.Join(dir, "report.html")
	}
	return filepath.Join(dir, "report_"+stem+".html")
}

func printReportUsage() {
	const usage = `faultbox report — build a self-contained HTML report from a .fb bundle

USAGE
  faultbox report <bundle.fb>                     # writes report_<bundle>.html alongside
  faultbox report <bundle.fb> --output <path>     # custom output path
  faultbox report <bundle.fb> --summary           # drop trace; smallest output (CI-friendly)
  faultbox report <bundle.fb> --full-events       # opt out of event downsampling
  faultbox report <bundle.fb> -o -                # write to stdout

The output is a single HTML file with all CSS, JS and bundle data
inlined. It opens in any browser with no network access. Data is
gzip+base64 encoded inside the HTML and decompressed in-browser via
DecompressionStream (Chrome 80+, Safari 16.4+, Firefox 113+).

MODES
  default        manifest + env + downsampled trace, gzip+base64.
                 Faults / violations / lifecycle events all kept; the
                 first 50 + last 50 syscalls per test kept; ±25
                 syscalls around each anchor kept. Everything else
                 dropped. Drill-down + swim-lane available.
  --full-events  no downsampling — every event from the bundle.
                 Use when you need full forensic detail.
  --summary      drop the trace entirely; matrix + tests + coverage
                 only. Typical output <100 KB — right for attaching
                 to CI runs, Slack threads, or Jira tickets.

EXAMPLES
  faultbox report run-2026-04-22-42.fb
  faultbox report run.fb --output report.html
  faultbox report run.fb --summary --output ci-summary.html
  faultbox report run.fb --full-events --output forensic.html
  faultbox report run.fb -o - | less
`
	fmt.Fprint(os.Stderr, usage)
}
