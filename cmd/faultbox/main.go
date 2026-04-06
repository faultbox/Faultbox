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
	_ "github.com/faultbox/Faultbox/internal/eventsource/decoder" // register decoders
	"github.com/faultbox/Faultbox/internal/generate"
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
	case "generate":
		return generateCmd(args[1:])
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
	// In multi-run mode (explicit --runs or auto-explore), group passing runs
	// into a compact summary.
	totalRuns := result.Pass + result.Fail
	if rcfg.Runs > 1 || (rcfg.ExploreMode == "all" && totalRuns > 1) {
		printMultiRunSummary(os.Stderr, result, totalRuns)
	} else {
		for _, tr := range result.Tests {
			printTraceSummary(os.Stderr, &tr)
		}
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

// printMultiRunSummary prints compact output for multi-run mode.
// Passing tests get one summary line; failing tests get full trace detail.
func printMultiRunSummary(w io.Writer, result *star.SuiteResult, totalRuns int) {
	// Group results by test name.
	type testGroup struct {
		name     string
		passed   int
		failed   int
		failedTR *star.TestResult // first failure (for detail)
	}
	groups := make(map[string]*testGroup)
	var order []string

	for i := range result.Tests {
		tr := &result.Tests[i]
		g, ok := groups[tr.Name]
		if !ok {
			g = &testGroup{name: tr.Name}
			groups[tr.Name] = g
			order = append(order, tr.Name)
		}
		if tr.Result == "pass" {
			g.passed++
		} else {
			g.failed++
			if g.failedTR == nil {
				g.failedTR = tr
			}
		}
	}

	// When --show fail filters out passing results, we may have no stored tests.
	// Use suite-level counts for the summary in that case.
	if len(order) == 0 && result.Pass > 0 {
		fmt.Fprintf(w, "\n--- PASS: all tests (%d/%d runs) ---\n", result.Pass, result.Pass)
		return
	}

	for _, name := range order {
		g := groups[name]
		if g.failed == 0 {
			// Compact: one line for all passing runs.
			fmt.Fprintf(w, "\n--- PASS: %s (%d/%d runs) ---\n", name, g.passed, totalRuns)
		} else {
			// Print full detail for the first failure.
			printTraceSummary(w, g.failedTR)
			if g.passed > 0 {
				fmt.Fprintf(w, "  (%d/%d runs passed before failure)\n", g.passed, g.passed+g.failed)
			}
		}
	}
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
		// Show op(syscall) when operation name is available.
		syscallDisplay := syscall
		if op, ok := ev.Fields["op"]; ok && op != "" {
			syscallDisplay = op + "(" + syscall + ")"
		}
		line := fmt.Sprintf("    #%d  %-12s %-16s %s", ev.Seq, ev.Service, syscallDisplay, decision)
		if label, ok := ev.Fields["label"]; ok && label != "" {
			line += fmt.Sprintf("  [%s]", label)
		}
		if path != "" {
			line += "  " + path
		}
		if lat, ok := ev.Fields["latency_ms"]; ok && lat != "0" {
			line += fmt.Sprintf("  (+%sms)", lat)
		}
		fmt.Fprintln(w, line)
	}

	// Show fault/trace scope timeline with per-scope hit counts.
	type faultScope struct {
		service string
		details string
		typ     string // "fault" or "trace"
		hits    int
		seq     int64
	}
	var scopes []faultScope
	for _, ev := range tr.Events {
		if ev.Type == "fault_applied" || ev.Type == "trace_applied" {
			var details []string
			for k, v := range ev.Fields {
				details = append(details, k+"="+v)
			}
			sort.Strings(details)
			typ := "fault"
			if ev.Type == "trace_applied" {
				typ = "trace"
			}
			scopes = append(scopes, faultScope{
				service: ev.Service,
				details: strings.Join(details, ", "),
				typ:     typ,
				seq:     ev.Seq,
			})
		}
	}

	// Count hits per scope: events between scope's seq and next scope or end.
	for i := range scopes {
		startSeq := scopes[i].seq
		var endSeq int64 = 1<<62 - 1
		// Find the next fault_removed/trace_removed for same service.
		for _, ev := range tr.Events {
			if (ev.Type == "fault_removed" || ev.Type == "trace_removed") &&
				ev.Service == scopes[i].service && ev.Seq > startSeq {
				endSeq = ev.Seq
				break
			}
		}
		for _, ev := range syscallEvents {
			if ev.Service == scopes[i].service && ev.Seq > startSeq && ev.Seq < endSeq {
				decision := ev.Fields["decision"]
				if decision != "allow" && decision != "allow (system path)" && decision != "" {
					scopes[i].hits++
				}
			}
		}
	}

	for _, scope := range scopes {
		label := "fault rule"
		if scope.typ == "trace" {
			label = "trace rule"
		}
		hitInfo := ""
		if scope.hits > 0 {
			hitInfo = fmt.Sprintf(" (%d hits)", scope.hits)
		}
		fmt.Fprintf(w, "  %s on %s: %s%s\n", label, scope.service, scope.details, hitInfo)
	}

	// Warn if faults were applied but never fired.
	hasFaults := false
	for _, s := range scopes {
		if s.typ == "fault" {
			hasFaults = true
			break
		}
	}
	if hasFaults && faultCount == 0 {
		fmt.Fprintln(w, "  WARNING: fault rules were installed but no injections fired")
		fmt.Fprintln(w, "    hint: the target may use a different syscall variant (e.g., pwrite64 instead of write)")
		fmt.Fprintln(w, "    hint: run with --debug to see all intercepted syscalls")
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
// Also: faultbox init --vscode generates VS Code autocomplete files.
func initCmd(args []string) int {
	// Check for --vscode first.
	for _, arg := range args {
		if arg == "--vscode" {
			return initVSCode()
		}
	}

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

// generateCmd handles: faultbox generate <file.star> [flags]
func generateCmd(args []string) int {
	var starFile, output, scenarioFilter, serviceFilter, category string
	dryRun := false

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasSuffix(args[i], ".star"):
			starFile = args[i]
		case args[i] == "--output" && i+1 < len(args):
			i++
			output = args[i]
		case strings.HasPrefix(args[i], "--output="):
			output = strings.TrimPrefix(args[i], "--output=")
		case args[i] == "--scenario" && i+1 < len(args):
			i++
			scenarioFilter = args[i]
		case strings.HasPrefix(args[i], "--scenario="):
			scenarioFilter = strings.TrimPrefix(args[i], "--scenario=")
		case args[i] == "--service" && i+1 < len(args):
			i++
			serviceFilter = args[i]
		case strings.HasPrefix(args[i], "--service="):
			serviceFilter = strings.TrimPrefix(args[i], "--service=")
		case args[i] == "--category" && i+1 < len(args):
			i++
			category = args[i]
		case strings.HasPrefix(args[i], "--category="):
			category = strings.TrimPrefix(args[i], "--category=")
		case args[i] == "--dry-run":
			dryRun = true
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(os.Stderr, `Usage: faultbox generate <file.star> [flags]

Generate failure scenarios from registered scenario() functions.

Flags:
  --output <file>        Write all mutations to a single file
  --scenario <name>      Generate only for this scenario
  --service <name>       Generate only for this dependency
  --category <cat>       Filter: network, disk, all (default: all)
  --dry-run              List mutations without generating code`)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			return 1
		}
	}

	if starFile == "" {
		fmt.Fprintln(os.Stderr, "error: .star file required\nusage: faultbox generate <file.star>")
		return 1
	}

	// Load the .star file.
	logger := logging.New(logging.Config{Level: slog.LevelWarn})
	rt := star.New(logger)
	if err := rt.LoadFile(starFile); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Analyze topology.
	analysis, err := generate.Analyze(rt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error analyzing topology: %v\n", err)
		return 1
	}

	if len(analysis.Scenarios) == 0 {
		fmt.Fprintln(os.Stderr, "no scenario() functions found — nothing to generate")
		fmt.Fprintln(os.Stderr, "hint: register happy paths with scenario(fn)")
		return 1
	}

	// Build failure matrix.
	mutations := generate.BuildMatrix(analysis)

	// Dry run — just show summary.
	if dryRun {
		fmt.Fprint(os.Stderr, generate.DryRun(mutations, analysis))
		return 0
	}

	opts := generate.GenerateOpts{
		Scenario: scenarioFilter,
		Service:  serviceFilter,
		Category: category,
		Source:   starFile,
	}

	if output != "" {
		// Single output file.
		code := generate.Generate(mutations, analysis, opts)
		if err := os.WriteFile(output, []byte(code), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", output, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d mutations)\n", output, len(mutations))
	} else {
		// Per-scenario files: <scenario>.faults.star
		perScenario := generate.GeneratePerScenario(mutations, analysis, opts)
		for scenario, code := range perScenario {
			fname := scenario + ".faults.star"
			if err := os.WriteFile(fname, []byte(code), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "error writing %s: %v\n", fname, err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", fname)
		}
	}

	return 0
}

// initVSCode generates VS Code autocomplete files for Starlark specs.
func initVSCode() int {
	// Create .vscode directory.
	if err := os.MkdirAll(".vscode", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating .vscode/: %v\n", err)
		return 1
	}

	// Write settings.json.
	// python.analysis.stubPath points to typings/ where __builtins__.pyi lives.
	// This makes Pylance treat all stub definitions as global builtins.
	settings := `{
    "files.associations": {
        "*.star": "python"
    },
    "python.analysis.stubPath": "typings",
    "python.analysis.diagnosticMode": "openFilesOnly",
    "python.analysis.typeCheckingMode": "off"
}
`
	if err := os.WriteFile(".vscode/settings.json", []byte(settings), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing settings.json: %v\n", err)
		return 1
	}

	// Write code snippets.
	if err := os.WriteFile(".vscode/faultbox.code-snippets", []byte(vscodeSnippets), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing snippets: %v\n", err)
		return 1
	}

	// Write type stubs as __builtins__.pyi so Pylance auto-loads them
	// as globals for all .star files — no import line needed.
	if err := os.MkdirAll("typings", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating typings/: %v\n", err)
		return 1
	}
	if err := os.WriteFile("typings/__builtins__.pyi", []byte(faultboxPyi), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing __builtins__.pyi: %v\n", err)
		return 1
	}

	fmt.Fprintln(os.Stderr, "wrote .vscode/settings.json")
	fmt.Fprintln(os.Stderr, "wrote .vscode/faultbox.code-snippets")
	fmt.Fprintln(os.Stderr, "wrote typings/__builtins__.pyi")
	fmt.Fprintln(os.Stderr, "\nVS Code autocomplete ready. Reload the window (Cmd+Shift+P → Reload).")
	return 0
}

var vscodeSnippets = `{
    "Faultbox Service": {
        "prefix": "svc",
        "scope": "python",
        "body": [
            "${1:name} = service(\"${1:name}\",",
            "    interface(\"main\", \"${2|http,tcp,postgres,redis,kafka,mysql,nats,grpc|}\", ${3:8080}),",
            "    ${4|binary =,image =,build =|} \"${5:path}\",",
            "    healthcheck = ${6|tcp,http|}(\"localhost:${3:8080}\"),",
            ")"
        ],
        "description": "Faultbox service declaration"
    },
    "Faultbox Test": {
        "prefix": "test",
        "scope": "python",
        "body": [
            "def test_${1:name}():",
            "    \"\"\"${2:description}\"\"\"",
            "    resp = ${3:api}.${4|get,post,put,delete|}(path=\"${5:/}\")",
            "    assert_eq(resp.status, ${6:200})"
        ],
        "description": "Faultbox test function"
    },
    "Faultbox Scenario": {
        "prefix": "scenario",
        "scope": "python",
        "body": [
            "def ${1:name}():",
            "    \"\"\"${2:Happy path description}\"\"\"",
            "    ${3:resp = api.post(path=\"/\", body=\"\")}",
            "    ${4:assert_eq(resp.status, 200)}",
            "",
            "scenario(${1:name})"
        ],
        "description": "Faultbox scenario (happy path for generator)"
    },
    "Faultbox Fault": {
        "prefix": "fault",
        "scope": "python",
        "body": [
            "def test_${1:name}():",
            "    \"\"\"${2:description}\"\"\"",
            "    def scenario():",
            "        resp = ${3:api}.${4|post,get|}(path=\"${5:/}\")",
            "        assert_true(resp.status >= 500, \"${6:expected error}\")",
            "    fault(${7:db}, ${8|write,connect,read,fsync|}=${9|deny,delay|}(\"${10:EIO}\", label=\"${11:label}\"), run=scenario)"
        ],
        "description": "Faultbox fault injection test"
    },
    "Faultbox Monitor": {
        "prefix": "monitor",
        "scope": "python",
        "body": [
            "monitor(lambda e: fail(\"${1:violation}\") if ${2:condition},",
            "    service=\"${3:service}\",",
            ")"
        ],
        "description": "Faultbox event monitor"
    },
    "Faultbox Observe": {
        "prefix": "observe",
        "scope": "python",
        "body": [
            "observe = [stdout(decoder=${1|json_decoder(),logfmt_decoder(),regex_decoder(pattern=\"\")|})]"
        ],
        "description": "Faultbox stdout observation"
    },
    "Faultbox Assert Eventually": {
        "prefix": "assert_ev",
        "scope": "python",
        "body": [
            "assert_eventually(where=lambda e: e.${1|service,type|} == \"${2:value}\")"
        ],
        "description": "Faultbox temporal assertion with lambda"
    }
}`

var faultboxPyi = `"""Faultbox Starlark type stubs for VS Code autocomplete."""

from typing import Any, Callable, Dict, List, Optional, Union

# ---------------------------------------------------------------------------
# Types
# ---------------------------------------------------------------------------

class service:
    """A service declaration."""
    name: str
    def __getattr__(self, name: str) -> 'interface_ref': ...

class interface:
    """An interface declaration."""
    def __init__(self, name: str, protocol: str, port: int, *, spec: str = ...) -> None: ...

class interface_ref:
    """Reference to a service interface."""
    addr: str
    host: str
    port: int
    internal_addr: str
    def get(self, *, path: str = "/", headers: Dict[str, str] = ...) -> 'response': ...
    def post(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    def put(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    def delete(self, *, path: str = "/", headers: Dict[str, str] = ...) -> 'response': ...
    def patch(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    def send(self, *, data: str) -> str: ...
    def query(self, *, sql: str) -> 'response': ...
    def exec(self, *, sql: str) -> 'response': ...
    def set(self, *, key: str, value: str) -> 'response': ...
    def get(self, *, key: str) -> 'response': ...
    def publish(self, *, topic: str = ..., subject: str = ..., data: str = ...) -> 'response': ...
    def consume(self, *, topic: str, group: str = ...) -> 'response': ...
    def call(self, *, method: str, body: str = "{}") -> 'response': ...

class response:
    """Response from a protocol step method."""
    status: int
    body: str
    data: Any
    ok: bool
    error: str
    duration_ms: int

class event:
    """Event in the trace log."""
    seq: int
    service: str
    type: str
    event_type: str
    data: Any
    fields: Dict[str, str]
    first: Optional['event']
    op: str
    decision: str
    label: str
    syscall: str
    path: str

class fault:
    """Fault definition returned by deny()/delay()/allow()."""
    ...

class healthcheck:
    """Healthcheck definition returned by tcp()/http()."""
    ...

class op:
    """Operation definition for named operations."""
    def __init__(self, *, syscalls: List[str], path: str = ...) -> None: ...

class decoder:
    """Decoder for event sources."""
    ...

class observe_source:
    """Event source for service observation."""
    ...

# ---------------------------------------------------------------------------
# Service & Interface
# ---------------------------------------------------------------------------

def service(
    name: str,
    binary: str = ...,
    *interfaces: interface,
    image: str = ...,
    build: str = ...,
    args: List[str] = ...,
    env: Dict[str, str] = ...,
    depends_on: List[service] = ...,
    volumes: Dict[str, str] = ...,
    healthcheck: healthcheck = ...,
    observe: List[observe_source] = ...,
    ops: Dict[str, op] = ...,
) -> service: ...

# ---------------------------------------------------------------------------
# Healthchecks
# ---------------------------------------------------------------------------

def tcp(addr: str, *, timeout: str = "10s") -> healthcheck: ...
def http(url: str, *, timeout: str = "10s") -> healthcheck: ...

# ---------------------------------------------------------------------------
# Fault Builders
# ---------------------------------------------------------------------------

def deny(errno: str, *, probability: str = "100%", label: str = ...) -> fault: ...
def delay(duration: str, *, probability: str = "100%", label: str = ...) -> fault: ...
def allow() -> fault: ...

# ---------------------------------------------------------------------------
# Fault Injection
# ---------------------------------------------------------------------------

def fault(svc: service, *, run: Callable, **syscall_faults: fault) -> Any: ...
def fault_start(svc: service, **syscall_faults: fault) -> None: ...
def fault_stop(svc: service) -> None: ...

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

def assert_true(condition: bool, msg: str = ...) -> None: ...
def assert_eq(a: Any, b: Any, msg: str = ...) -> None: ...

def assert_eventually(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> None: ...

def assert_never(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> None: ...

def assert_before(
    *,
    first: Union[Dict[str, str], Callable[[event], bool]] = ...,
    then: Union[Dict[str, str], Callable[[event], bool]] = ...,
) -> None: ...

# ---------------------------------------------------------------------------
# Events & Monitoring
# ---------------------------------------------------------------------------

def events(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> List[event]: ...

def monitor(
    callback: Callable[[event], None],
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
) -> None: ...

# ---------------------------------------------------------------------------
# Concurrency
# ---------------------------------------------------------------------------

def parallel(*callables: Callable) -> List[Any]: ...
def nondet(*services: service) -> None: ...

# ---------------------------------------------------------------------------
# Tracing
# ---------------------------------------------------------------------------

def trace(svc: service, *, syscalls: List[str], run: Callable) -> Any: ...
def trace_start(svc: service, *, syscalls: List[str]) -> None: ...
def trace_stop(svc: service) -> None: ...

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

def partition(svc_a: service, svc_b: service, *, run: Callable) -> Any: ...

# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------

def scenario(fn: Callable) -> None: ...

# ---------------------------------------------------------------------------
# Event Sources & Decoders
# ---------------------------------------------------------------------------

def stdout(*, decoder: decoder = ...) -> observe_source: ...
def json_decoder() -> decoder: ...
def logfmt_decoder() -> decoder: ...
def regex_decoder(*, pattern: str) -> decoder: ...

# ---------------------------------------------------------------------------
# Starlark builtins
# ---------------------------------------------------------------------------

def print(*args: Any) -> None: ...
def fail(msg: str) -> None: ...
def load(module: str, *symbols: str) -> None: ...
`

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  faultbox run [flags] <binary> [args...]    Run a single service
  faultbox test [flags] <file.star>          Run multi-service tests
  faultbox generate <file.star> [flags]     Generate failure scenarios
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
  --runs <N>               Run each test N times (compact summary, stop on first failure)
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
