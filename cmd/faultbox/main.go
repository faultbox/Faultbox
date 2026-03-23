package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/seccomp"
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
	// Parse args: faultbox run [flags] <binary> [args...]
	args := os.Args[1:]

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printUsage()
		return 0
	}

	if args[0] != "run" {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		return 1
	}
	args = args[1:]

	// Parse flags before the binary path.
	logFormat := logging.FormatAuto
	logLevel := slog.LevelInfo
	var faultSpecs []string

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
			args = args[1:] // consume the value
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

	// Parse fault rules.
	faultRules, err := engine.ParseFaultRules(faultSpecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	binary := args[0]
	binaryArgs := args[1:]

	// Set up logging.
	logger := logging.New(logging.Config{
		Format: logFormat,
		Level:  logLevel,
	})

	// Set up engine.
	eng := engine.New(logger)

	// Handle Ctrl+C.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ctx = logging.NewContext(ctx, logger)

	// Build session config.
	cfg := engine.SessionConfig{
		Binary:     binary,
		Args:       binaryArgs,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Namespaces: engine.DefaultNamespaces(),
		FaultRules: faultRules,
	}

	// When using fault injection, namespaces are not used (the shim handles isolation).
	if len(faultRules) > 0 {
		cfg.Namespaces = engine.NamespaceConfig{} // disable namespace flags
	}

	// Run the target.
	result, err := eng.Run(ctx, cfg)
	if err != nil {
		logger.Error("session failed", slog.String("error", err.Error()))
		return 1
	}

	return result.ExitCode
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: faultbox run [flags] <binary> [args...]

Flags:
  --log-format=console   Force colored console output
  --log-format=json      Force JSON lines output
  --debug                Enable debug logging
  --fault "spec"         Inject fault: syscall=ERRNO:PROB%

Fault examples:
  --fault "openat=ENOENT:50%"       Fail 50% of openat() with ENOENT
  --fault "write=EIO:100%"          Fail every write() with EIO
  --fault "connect=ECONNREFUSED:10%"

Example:
  faultbox run ./my-service --port 8080
  faultbox run --fault "write=EIO:20%" ./my-service`)
}
