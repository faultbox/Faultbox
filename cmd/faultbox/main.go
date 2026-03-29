package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
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
	case "init":
		return initCmd(args[1:])
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
		Binary:             args[0],
		Args:               args[1:],
		Env:                envVars,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
		Namespaces:         engine.DefaultNamespaces(),
		FaultRules:         faultRules,
		ExternalListenerFd: -1, // not external — launch the binary
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
	var runs int
	var seed int64 = -1 // -1 = not set
	showFilter := "all" // common output filter: "all", "fail"
	virtualTime := false
	exploreMode := ""

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
		case strings.HasPrefix(args[0], "--runs="):
			fmt.Sscanf(strings.TrimPrefix(args[0], "--runs="), "%d", &runs)
		case args[0] == "--runs" && len(args) > 1:
			fmt.Sscanf(args[1], "%d", &runs)
			args = args[1:]
		case strings.HasPrefix(args[0], "--seed="):
			fmt.Sscanf(strings.TrimPrefix(args[0], "--seed="), "%d", &seed)
		case args[0] == "--seed" && len(args) > 1:
			fmt.Sscanf(args[1], "%d", &seed)
			args = args[1:]
		case args[0] == "--show=fail" || args[0] == "--show=failures":
			showFilter = "fail"
		case args[0] == "--show=all":
			showFilter = "all"
		case args[0] == "--virtual-time":
			virtualTime = true
		case strings.HasPrefix(args[0], "--explore="):
			exploreMode = strings.TrimPrefix(args[0], "--explore=")
		case args[0] == "--explore" && len(args) > 1:
			exploreMode = args[1]
			args = args[1:]
		case args[0] == "--show" && len(args) > 1:
			showFilter = args[1]
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
		var seedPtr *uint64
		if seed >= 0 {
			u := uint64(seed)
			seedPtr = &u
		}
		// Default runs for explore=sample when not specified.
		if exploreMode == "sample" && runs == 0 {
			runs = 100
		}
		rcfg := star.RunConfig{
			Filter:      testFilter,
			Seed:        seedPtr,
			Runs:        runs,
			FailOnly:    showFilter == "fail",
			VirtualTime: virtualTime,
			ExploreMode: exploreMode,
		}
		return testStarCmd(starFile, rcfg, outputPath, shivizPath, normalizePath, logFormat, logLevel)
	}

	return testYAMLCmd(configPath, specPath, outputPath, logFormat, logLevel)
}

// testStarCmd runs tests from a .star file.
func testStarCmd(starFile string, rcfg star.RunConfig, outputPath, shivizPath, normalizePath string, logFormat logging.Format, logLevel slog.Level) int {
	logger := logging.New(logging.Config{Format: logFormat, Level: logLevel})
	rt := star.New(logger)

	if err := rt.LoadFile(starFile); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	result, err := rt.RunAll(ctx, rcfg)
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
		if err := star.WriteTraceResults(outputPath, starFile, result); err != nil {
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
	fmt.Fprintf(w, "\n--- %s: %s (%dms, seed=%d) ---\n", status, tr.Name, tr.DurationMs, tr.Seed)

	if tr.Result == "fail" && tr.Reason != "" {
		fmt.Fprintf(w, "  reason: %s\n", tr.Reason)
		fmt.Fprintf(w, "  replay: faultbox test <file> --test %s --seed %d\n",
			strings.TrimPrefix(tr.Name, "test_"), tr.Seed)
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

	// Collect per-service syscall stats and fault hit counts.
	type svcStats struct {
		total     int
		faultHits int
		syscalls  map[string]int
	}
	stats := make(map[string]*svcStats)
	for _, ev := range syscallEvents {
		svc := ev.Service
		if stats[svc] == nil {
			stats[svc] = &svcStats{syscalls: make(map[string]int)}
		}
		stats[svc].total++
		sc := ev.Fields["syscall"]
		stats[svc].syscalls[sc]++
		decision := ev.Fields["decision"]
		if decision != "allow" && decision != "allow (system path)" && decision != "" {
			stats[svc].faultHits++
		}
	}

	// Print compact syscall trace.
	fmt.Fprintf(w, "  syscall trace (%d events):\n", len(syscallEvents))

	// Show fault events.
	var faultCount int
	for _, ev := range syscallEvents {
		decision := ev.Fields["decision"]
		syscall := ev.Fields["syscall"]
		path := ev.Fields["path"]

		if decision == "allow" || decision == "allow (system path)" {
			continue
		}

		faultCount++
		line := fmt.Sprintf("    #%d  %-12s %-10s %s", ev.Seq, ev.Service, syscall, decision)
		if path != "" {
			line += "  " + path
		}
		if lat, ok := ev.Fields["latency_ms"]; ok && lat != "0" {
			line += fmt.Sprintf("  (+%sms)", lat)
		}
		fmt.Fprintln(w, line)
	}

	// Show fault_applied events with details (helps diagnose missing faults).
	for _, ev := range tr.Events {
		if ev.Type == "fault_applied" {
			var details []string
			for k, v := range ev.Fields {
				details = append(details, k+"="+v)
			}
			if len(details) > 0 {
				sort.Strings(details)
				fmt.Fprintf(w, "  fault applied to %s: %s\n", ev.Service, strings.Join(details, ", "))
			}
		}
	}

	// Warn if faults were applied but never fired.
	if faultCount == 0 && tr.Result == "fail" {
		hasFaults := false
		for _, ev := range tr.Events {
			if ev.Type == "fault_applied" {
				hasFaults = true
				break
			}
		}
		if hasFaults {
			fmt.Fprintln(w, "  ⚠ faults were applied but no syscalls were denied/delayed")
			fmt.Fprintln(w, "    hint: the target process may use a different syscall (e.g., pwrite64 instead of write)")
			fmt.Fprintln(w, "    hint: run with --debug to see all intercepted syscalls")
		}
	}

	// Show per-service syscall breakdown (when test fails or debug).
	if tr.Result == "fail" {
		fmt.Fprintln(w, "  per-service syscall summary:")
		for svc, st := range stats {
			var scList []string
			for sc, n := range st.syscalls {
				scList = append(scList, fmt.Sprintf("%s:%d", sc, n))
			}
			sort.Strings(scList)
			fmt.Fprintf(w, "    %-12s %d total, %d faulted  [%s]\n", svc, st.total, st.faultHits, strings.Join(scList, " "))
		}
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

// initCmd handles: faultbox init [flags] <binary>
// Generates a starter .star file for a service.
func initCmd(args []string) int {
	name := "myapp"
	port := "8080"
	protocol := "http"
	output := ""
	var binary string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		case args[i] == "--port" && i+1 < len(args):
			i++
			port = args[i]
		case strings.HasPrefix(args[i], "--port="):
			port = strings.TrimPrefix(args[i], "--port=")
		case args[i] == "--protocol" && i+1 < len(args):
			i++
			protocol = args[i]
		case strings.HasPrefix(args[i], "--protocol="):
			protocol = strings.TrimPrefix(args[i], "--protocol=")
		case args[i] == "--output" && i+1 < len(args):
			i++
			output = args[i]
		case strings.HasPrefix(args[i], "--output="):
			output = strings.TrimPrefix(args[i], "--output=")
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(os.Stderr, `Usage: faultbox init [flags] <binary>

Generate a starter .star file for a service.

Flags:
  --name <name>          Service name (default: myapp)
  --port <port>          Port number (default: 8080)
  --protocol http|tcp    Protocol (default: http)
  --output <file>        Write to file instead of stdout`)
			return 0
		case !strings.HasPrefix(args[i], "-"):
			binary = args[i]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			return 1
		}
	}

	if binary == "" {
		fmt.Fprintln(os.Stderr, "error: binary path is required\nusage: faultbox init [flags] <binary>")
		return 1
	}

	// Build healthcheck line based on protocol.
	healthcheck := fmt.Sprintf(`%s("localhost:%s")`, protocol, port)
	if protocol == "http" {
		healthcheck = fmt.Sprintf(`http("localhost:%s/health")`, port)
	}

	// Build example test based on protocol.
	var testExample string
	if protocol == "http" {
		testExample = fmt.Sprintf(`def test_health_check():
    """Verify service starts and responds."""
    resp = %s.get(path="/health")
    assert_eq(resp.status, 200)

# def test_write_fault():
#     """Example: inject a write fault."""
#     def scenario():
#         resp = %s.post(path="/data", body='{"key":"value"}')
#         assert_true(resp.status != 200, "expected failure under fault")
#     fault(%s, write=deny("EIO"), run=scenario)`, name, name, name)
	} else {
		testExample = fmt.Sprintf(`def test_ping():
    """Verify service starts and responds."""
    resp = %s.main.send(data="PING")
    assert_eq(resp, "PONG")

# def test_write_fault():
#     """Example: inject a write fault."""
#     def scenario():
#         resp = %s.main.send(data="PING")
#         assert_eq(resp, "PONG")
#     fault(%s, write=deny("EIO"), run=scenario)`, name, name, name)
	}

	content := fmt.Sprintf(`# faultbox.star — generated by faultbox init
#
# Run all:   faultbox test faultbox.star
# Run one:   faultbox test faultbox.star --test health_check
# Trace:     faultbox test faultbox.star --output trace.json

%s = service("%s",
    "%s",
    interface("main", "%s", %s),
    env = {"PORT": "%s"},
    healthcheck = %s,
)

%s
`, name, name, binary, protocol, port, port, healthcheck, testExample)

	if output != "" {
		if err := os.WriteFile(output, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", output, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", output)
	} else {
		fmt.Print(content)
	}
	return 0
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  faultbox run [flags] <binary> [args...]    Run a single service
  faultbox test [flags] <file.star>          Run multi-service tests
  faultbox init [flags] <binary>             Generate starter .star file
  faultbox diff <trace1> <trace2>            Compare normalized traces

Run flags:
  --log-format=console   Force colored console output
  --log-format=json      Force JSON lines output
  --debug                Enable debug logging
  --fault "spec"         Inject fault: syscall=ACTION:PROB%[:PATH][:TRIGGER]
  --fs-fault "spec"      Filesystem fault (maps op to syscalls)
  --env KEY=VALUE        Set environment variable for the target

Test flags:
  --test <name>            Run only matching test
  --runs <N>               Run each test N times (stop on first failure)
  --seed <N>               Deterministic seed for replay
  --show all|fail          Filter output (default: all)
  --output results.json    Write JSON trace results
  --shiviz trace.shiviz    Write ShiViz visualization
  --normalize trace.norm   Write normalized trace for diff
  --log-format=console     Force colored console output
  --debug                  Enable debug logging

Init flags:
  --name <name>            Service name (default: myapp)
  --port <port>            Port number (default: 8080)
  --protocol http|tcp      Protocol (default: http)
  --output <file>          Write to file instead of stdout

Examples:
  faultbox run ./my-service
  faultbox run --fault "write=EIO:20%" -- ./my-service --port 8080
  faultbox test faultbox.star
  faultbox test faultbox.star --output trace.json --runs 100 --show fail
  faultbox init --name orders --port 8080 ./order-svc`)
}
