package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/faultbox/Faultbox/internal/config"
	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/seccomp"
	"github.com/faultbox/Faultbox/internal/star"
)

func main() {
	// If we're the re-exec'd seccomp shim child, run that path and exit.
	if seccomp.IsShimChild() {
		if err := seccomp.RunShimChild(); err != nil {
			fmt.Fprintf(os.Stderr, "faultbox shim: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0) // unreachable if Exec succeeds
	}

	os.Exit(run())
}

func run() int {
	args := os.Args[1:]

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage()
		return 0
	}

	switch args[0] {
	case "run":
		return runCmd(args[1:])
	case "test":
		return testCmd(args[1:])
	case "diff":
		return diffCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		return 1
	}
}

// runCmd handles: faultbox run [flags] <binary> [args...]
func runCmd(args []string) int {
	logFormat := logging.FormatAuto
	logLevel := slog.LevelInfo
	var faultSpecs []string
	var envVars []string

	for len(args) > 0 && len(args[0]) > 0 && args[0][0] == '-' {
		switch {
		case args[0] == "--log-format=console":
			logFormat = logging.FormatConsole
		case args[0] == "--log-format=json":
			logFormat = logging.FormatJSON
		case args[0] == "--debug":
			logLevel = slog.LevelDebug
		case strings.HasPrefix(args[0], "--fault="):
			faultSpecs = append(faultSpecs, strings.TrimPrefix(args[0], "--fault="))
		case args[0] == "--fault" && len(args) > 1:
			faultSpecs = append(faultSpecs, args[1])
			args = args[1:]
		case strings.HasPrefix(args[0], "--fs-fault="):
			faultSpecs = append(faultSpecs, expandFsFault(strings.TrimPrefix(args[0], "--fs-fault="))...)
		case args[0] == "--fs-fault" && len(args) > 1:
			faultSpecs = append(faultSpecs, expandFsFault(args[1])...)
			args = args[1:]
		case strings.HasPrefix(args[0], "--env="):
			envVars = append(envVars, strings.TrimPrefix(args[0], "--env="))
		case args[0] == "--env" && len(args) > 1:
			envVars = append(envVars, args[1])
			args = args[1:]
		case args[0] == "--":
			args = args[1:]
			goto doneFlags
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[0])
			return 1
		}
		args = args[1:]
	}
doneFlags:

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: no binary specified")
		printUsage()
		return 1
	}

	faultRules, err := engine.ParseFaultRules(faultSpecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	logger := logging.New(logging.Config{Format: logFormat, Level: logLevel})
	eng := engine.New(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx = logging.NewContext(ctx, logger)

	cfg := engine.SessionConfig{
		Binary:     args[0],
		Args:       args[1:],
		Env:        envVars,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Namespaces: engine.DefaultNamespaces(),
		FaultRules: faultRules,
	}

	result, err := eng.Run(ctx, cfg)
	if err != nil {
		logger.Error("session failed", slog.String("error", err.Error()))
		return 1
	}
	return result.ExitCode
}

// testCmd handles:
//   faultbox test faultbox.star [--test name] [--output results.json]
//   faultbox test --config faultbox.yaml --spec spec.yaml [--output results.json]
func testCmd(args []string) int {
	logFormat := logging.FormatAuto
	logLevel := slog.LevelInfo
	var configPath, specPath, outputPath, shivizPath, normalizePath, testFilter string
	var starFile string

	for len(args) > 0 {
		switch {
		case args[0] == "--log-format=console":
			logFormat = logging.FormatConsole
		case args[0] == "--log-format=json":
			logFormat = logging.FormatJSON
		case args[0] == "--debug":
			logLevel = slog.LevelDebug
		case strings.HasPrefix(args[0], "--config="):
			configPath = strings.TrimPrefix(args[0], "--config=")
		case args[0] == "--config" && len(args) > 1:
			configPath = args[1]
			args = args[1:]
		case strings.HasPrefix(args[0], "--spec="):
			specPath = strings.TrimPrefix(args[0], "--spec=")
		case args[0] == "--spec" && len(args) > 1:
			specPath = args[1]
			args = args[1:]
		case strings.HasPrefix(args[0], "--output="):
			outputPath = strings.TrimPrefix(args[0], "--output=")
		case args[0] == "--output" && len(args) > 1:
			outputPath = args[1]
			args = args[1:]
		case strings.HasPrefix(args[0], "--shiviz="):
			shivizPath = strings.TrimPrefix(args[0], "--shiviz=")
		case args[0] == "--shiviz" && len(args) > 1:
			shivizPath = args[1]
			args = args[1:]
		case strings.HasPrefix(args[0], "--normalize="):
			normalizePath = strings.TrimPrefix(args[0], "--normalize=")
		case args[0] == "--normalize" && len(args) > 1:
			normalizePath = args[1]
			args = args[1:]
		case strings.HasPrefix(args[0], "--test="):
			testFilter = strings.TrimPrefix(args[0], "--test=")
		case args[0] == "--test" && len(args) > 1:
			testFilter = args[1]
			args = args[1:]
		case strings.HasSuffix(args[0], ".star"):
			starFile = args[0]
		case strings.HasSuffix(args[0], ".yaml") || strings.HasSuffix(args[0], ".yml"):
			if configPath == "" {
				configPath = args[0]
			} else {
				specPath = args[0]
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[0])
			return 1
		}
		args = args[1:]
	}

	if starFile != "" {
		return testStarCmd(starFile, testFilter, outputPath, shivizPath, normalizePath, logFormat, logLevel)
	}

	return testYAMLCmd(configPath, specPath, outputPath, logFormat, logLevel)
}

// testStarCmd runs tests from a .star file.
func testStarCmd(starFile, testFilter, outputPath, shivizPath, normalizePath string, logFormat logging.Format, logLevel slog.Level) int {
	logger := logging.New(logging.Config{Format: logFormat, Level: logLevel})
	rt := star.New(logger)

	if err := rt.LoadFile(starFile); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	result, err := rt.RunAll(ctx, testFilter)
	if err != nil {
		logger.Error("test suite failed", slog.String("error", err.Error()))
		return 1
	}

	// Print trace summary per test.
	for _, tr := range result.Tests {
		printTraceSummary(os.Stderr, &tr)
	}

	// Write results if requested.
	if outputPath != "" {
		if err := star.WriteTraceResults(outputPath, result); err != nil {
			logger.Error("failed to write results", slog.String("error", err.Error()))
			return 1
		}
		logger.Info("results written", slog.String("path", outputPath))
	}

	// Write ShiViz trace if requested.
	if shivizPath != "" {
		if err := star.WriteShiVizTrace(shivizPath, result); err != nil {
			logger.Error("failed to write shiviz trace", slog.String("error", err.Error()))
			return 1
		}
		logger.Info("shiviz trace written", slog.String("path", shivizPath))
	}

	// Write normalized trace if requested.
	if normalizePath != "" {
		if err := star.WriteNormalizedTrace(normalizePath, result); err != nil {
			logger.Error("failed to write normalized trace", slog.String("error", err.Error()))
			return 1
		}
		logger.Info("normalized trace written", slog.String("path", normalizePath))
	}

	// Print summary.
	fmt.Fprintf(os.Stderr, "\n%d passed, %d failed\n", result.Pass, result.Fail)

	if result.Fail > 0 {
		return 2
	}
	return 0
}

// printTraceSummary prints a human-readable syscall trace for a test.
func printTraceSummary(w io.Writer, tr *star.TestResult) {
	// Status marker.
	status := "PASS"
	if tr.Result == "fail" {
		status = "FAIL"
	}
	fmt.Fprintf(w, "\n--- %s: %s (%dms) ---\n", status, tr.Name, tr.DurationMs)

	if tr.Result == "fail" && tr.Reason != "" {
		fmt.Fprintf(w, "  reason: %s\n", tr.Reason)
	}

	// Count syscall events (skip lifecycle events).
	var syscallEvents []star.Event
	for _, ev := range tr.Events {
		if ev.Type == "syscall" {
			syscallEvents = append(syscallEvents, ev)
		}
	}

	if len(syscallEvents) == 0 {
		return
	}

	// Print compact syscall trace.
	fmt.Fprintf(w, "  syscall trace (%d events):\n", len(syscallEvents))
	for _, ev := range syscallEvents {
		decision := ev.Fields["decision"]
		syscall := ev.Fields["syscall"]
		path := ev.Fields["path"]

		// Only show interesting events (faults) in default mode.
		if decision == "allow" || decision == "allow (system path)" {
			continue
		}

		line := fmt.Sprintf("    #%d  %-12s %-10s %s", ev.Seq, ev.Service, syscall, decision)
		if path != "" {
			line += "  " + path
		}
		if lat, ok := ev.Fields["latency_ms"]; ok && lat != "0" {
			line += fmt.Sprintf("  (+%sms)", lat)
		}
		fmt.Fprintln(w, line)
	}
}

// testYAMLCmd runs tests from YAML config + spec files (legacy path).
// diffCmd handles: faultbox diff trace1.norm trace2.norm
// Compares two normalized trace files and reports differences.
func diffCmd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: faultbox diff <trace1.norm> <trace2.norm>")
		return 1
	}

	data1, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", args[0], err)
		return 1
	}
	data2, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", args[1], err)
		return 1
	}

	diff := star.DiffTraces(string(data1), string(data2))
	if diff == "" {
		fmt.Println("traces are identical")
		return 0
	}

	fmt.Println(diff)
	return 2
}

func testYAMLCmd(configPath, specPath, outputPath string, logFormat logging.Format, logLevel slog.Level) int {
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (or pass a .star file)")
		return 1
	}
	if specPath == "" {
		fmt.Fprintln(os.Stderr, "error: --spec is required (or pass a .star file)")
		return 1
	}

	topo, err := config.LoadTopology(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	spec, err := config.LoadSpec(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	logger := logging.New(logging.Config{Format: logFormat, Level: logLevel})
	eng := engine.New(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx = logging.NewContext(ctx, logger)

	result, err := eng.RunSimulation(ctx, topo, spec)
	if err != nil {
		logger.Error("simulation failed", slog.String("error", err.Error()))
		return 1
	}

	if outputPath != "" {
		if err := engine.WriteResults(outputPath, result); err != nil {
			logger.Error("failed to write results", slog.String("error", err.Error()))
			return 1
		}
		logger.Info("results written", slog.String("path", outputPath))
	}

	if result.Fail > 0 {
		return 2
	}
	return 0
}

// expandFsFault maps --fs-fault operation names to the correct syscall fault specs.
func expandFsFault(spec string) []string {
	parts := strings.SplitN(spec, "=", 2)
	if len(parts) != 2 {
		return []string{spec}
	}
	op := strings.TrimSpace(parts[0])
	rest := parts[1]

	syscalls, ok := fsFaultMap[op]
	if !ok {
		return []string{spec}
	}

	result := make([]string, len(syscalls))
	for i, sc := range syscalls {
		result[i] = sc + "=" + rest
	}
	return result
}

var fsFaultMap = map[string][]string{
	"open":   {"openat"},
	"read":   {"read", "readv"},
	"write":  {"write", "writev"},
	"sync":   {"fsync"},
	"fsync":  {"fsync"},
	"mkdir":  {"mkdirat"},
	"delete": {"unlinkat"},
	"stat":   {"fstatat"},
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  faultbox run [flags] <binary> [args...]    Run a single service
  faultbox test [flags]                      Run multi-service traces

Run flags:
  --log-format=console   Force colored console output
  --log-format=json      Force JSON lines output
  --debug                Enable debug logging
  --fault "spec"         Inject fault: syscall=ACTION:PROB%[:PATH][:TRIGGER]
  --fs-fault "spec"      Filesystem fault (maps op to syscalls)
  --env KEY=VALUE        Set environment variable for the target

Test flags:
  --config faultbox.yaml   Topology file (required)
  --spec spec.yaml         Spec file with traces (required)
  --output results.json    Write structured results to file
  --log-format=console     Force colored console output
  --log-format=json        Force JSON lines output
  --debug                  Enable debug logging

Examples:
  faultbox run ./my-service
  faultbox run --fault "write=EIO:20%" -- ./my-service --port 8080
  faultbox test --config faultbox.yaml --spec spec.yaml --output results.json`)
}
