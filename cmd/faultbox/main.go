package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/logging"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Parse args: faultbox run [--log-format=console|json] <binary> [args...]
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

	for len(args) > 0 && args[0][0] == '-' {
		switch args[0] {
		case "--log-format=console":
			logFormat = logging.FormatConsole
		case "--log-format=json":
			logFormat = logging.FormatJSON
		case "--debug":
			logLevel = slog.LevelDebug
		case "--":
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

	// Run the target.
	result, err := eng.Run(ctx, engine.SessionConfig{
		Binary:     binary,
		Args:       binaryArgs,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Namespaces: engine.DefaultNamespaces(),
	})
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

Example:
  faultbox run ./my-service --port 8080`)
}
