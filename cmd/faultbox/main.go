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
	// If we're the re-exec'd shim child, run that path and exit.
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
			args = args[1:] // consume the value
		case strings.HasPrefix(args[0], "--fs-fault="):
			faultSpecs = append(faultSpecs, expandFsFault(strings.TrimPrefix(args[0], "--fs-fault="))...)
		case args[0] == "--fs-fault" && len(args) > 1:
			faultSpecs = append(faultSpecs, expandFsFault(args[1])...)
			args = args[1:]
		case strings.HasPrefix(args[0], "--env="):
			envVars = append(envVars, strings.TrimPrefix(args[0], "--env="))
		case args[0] == "--env" && len(args) > 1:
			envVars = append(envVars, args[1])
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

	// Build session config — always use namespaces + optional faults.
	cfg := engine.SessionConfig{
		Binary:     binary,
		Args:       binaryArgs,
		Env:        envVars,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Namespaces: engine.DefaultNamespaces(),
		FaultRules: faultRules,
	}

	// Run the target.
	result, err := eng.Run(ctx, cfg)
	if err != nil {
		logger.Error("session failed", slog.String("error", err.Error()))
		return 1
	}

	return result.ExitCode
}

// expandFsFault maps --fs-fault operation names to the correct syscall fault specs.
// e.g., "open=ENOENT:100%:/data/*" → ["openat=ENOENT:100%:/data/*"]
//
//	"sync=EIO:100%:after=2"  → ["fsync=EIO:100%:after=2"]
func expandFsFault(spec string) []string {
	parts := strings.SplitN(spec, "=", 2)
	if len(parts) != 2 {
		return []string{spec} // let the parser produce the error
	}
	op := strings.TrimSpace(parts[0])
	rest := parts[1]

	syscalls, ok := fsFaultMap[op]
	if !ok {
		// Pass through as-is — the parser will validate the syscall name.
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
	fmt.Fprintln(os.Stderr, `Usage: faultbox run [flags] <binary> [args...]

Flags:
  --log-format=console   Force colored console output
  --log-format=json      Force JSON lines output
  --debug                Enable debug logging
  --fault "spec"         Inject fault: syscall=ACTION:PROB%[:PATH][:TRIGGER]
  --fs-fault "spec"      Filesystem fault (maps op to syscalls)
  --env KEY=VALUE        Set environment variable for the target

Fault examples:
  --fault "openat=ENOENT:50%"             Fail 50% of openat()
  --fault "openat=ENOENT:100%:/data/*"    Fail opens under /data/ only
  --fault "write=EIO:100%"                Fail every write()
  --fault "connect=delay:200ms:100%"      Delay connect() by 200ms
  --fault "fsync=EIO:100%:after=2"        Fail fsyncs after first 2 succeed
  --fault "openat=ENOENT:100%:nth=3"      Fail only the 3rd openat()
  --fs-fault "sync=EIO:100%:after=2"      Same as --fault "fsync=EIO:100%:after=2"

Example:
  faultbox run ./my-service
  faultbox run --env DB_URL=postgres://localhost/db ./my-service
  faultbox run --fault "write=EIO:20%" -- ./my-service --port 8080`)
}
