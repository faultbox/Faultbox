package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/faultbox/Faultbox/internal/gvisor"
)

// `faultbox setup-trace` — one-time host registration for watch() (RFC-056 M1).
//
// Deliberately a separate command rather than something `faultbox test` does on
// demand. It writes /etc/docker/daemon.json, and applying that requires a
// daemon restart which stops every container on the machine. Neither is a side
// effect a test run should have, and neither is something a user should
// discover after the fact.
//
// So: this command changes files and reports exactly what it changed; the user
// performs the restart, knowing what it is for.

func setupTraceCmd(args []string) int {
	var (
		check     bool
		withRead  bool
		withClose bool
		withConn  bool
		sinkPath  string
	)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--check":
			check = true
		case a == "--with-read":
			withRead = true
		case a == "--with-close":
			withClose = true
		case a == "--with-connect":
			withConn = true
		case strings.HasPrefix(a, "--sink="):
			sinkPath = strings.TrimPrefix(a, "--sink=")
		case a == "--sink" && i+1 < len(args):
			i++
			sinkPath = args[i]
		case a == "-h" || a == "--help":
			printSetupTraceUsage(os.Stdout)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "error: unknown argument %q\n\n", a)
			printSetupTraceUsage(os.Stderr)
			return 1
		}
	}

	opts := gvisor.SetupOptions{SinkPath: sinkPath}
	if withRead {
		opts.Extra = append(opts.Extra, gvisor.PointSetRead)
	}
	if withClose {
		opts.Extra = append(opts.Extra, gvisor.PointSetClose)
	}
	if withConn {
		opts.Extra = append(opts.Extra, gvisor.PointSetConnect)
	}

	if check {
		changes, err := gvisor.Plan(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		if gvisor.Unchanged(changes) {
			fmt.Println("Filesystem observation is registered and up to date.")
			printChanges(os.Stdout, changes, true)
			return 0
		}
		fmt.Println("Filesystem observation is NOT fully registered. `faultbox setup-trace` would:")
		fmt.Println()
		printChanges(os.Stdout, changes, false)
		// Exit 1 so a provisioning script can gate on it.
		return 1
	}

	changes, err := gvisor.Apply(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if os.Geteuid() != 0 {
			fmt.Fprintf(os.Stderr, "\nThis writes %s and %s, so it needs root:\n    sudo faultbox setup-trace\n",
				gvisor.TraceConfigPath, gvisor.DaemonJSONPath)
		}
		return 1
	}

	if gvisor.Unchanged(changes) {
		fmt.Println("Already registered — nothing changed.")
		printChanges(os.Stdout, changes, true)
		return 0
	}

	fmt.Println("Registered filesystem observation.")
	fmt.Println()
	printChanges(os.Stdout, changes, false)
	printRestartInstruction(os.Stdout, withRead)
	return 0
}

func printChanges(w io.Writer, changes []gvisor.Change, quiet bool) {
	for _, c := range changes {
		switch c.Kind {
		case "unchanged":
			if !quiet {
				fmt.Fprintf(w, "  %-32s unchanged\n", c.Path)
				continue
			}
			fmt.Fprintf(w, "  %-32s ok\n", c.Path)
		case "create":
			fmt.Fprintf(w, "  %-32s created\n", c.Path)
		default:
			fmt.Fprintf(w, "  %-32s updated\n", c.Path)
		}
		for _, d := range c.Detail {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
}

// printRestartInstruction explains the one thing this command deliberately
// does not do, and why the user has to choose when it happens.
func printRestartInstruction(w io.Writer, withRead bool) {
	fmt.Fprint(w, `
Docker has not picked this up yet. The daemon reads daemon.json only when it
starts, so the faultbox-trace runtime does not exist until you restart it:

    sudo systemctl restart docker

That restart stops every container on this host, which is why it is left to
you rather than done automatically. Pick a moment when that is safe.

Afterwards, nothing else is required:
  - Faultbox never edits your Docker configuration during a test run.
  - A registration left in place costs nothing while Faultbox is idle — the
    trace session tolerates a missing sink, so unrelated gVisor containers
    start normally whether or not Faultbox is running.

Verify with:
    faultbox setup-trace --check
`)
	if withRead {
		fmt.Fprint(w, `
Note on --with-read: read tracing roughly doubles trace traffic. Measured on a
read-heavy workload it produced 1,488 dropped points where the default set
dropped none, and dropped points fail a test — an audit that missed operations
cannot prove "never". If specs start failing on drops, this is the first thing
to turn off.
`)
	}
}

func printSetupTraceUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: faultbox setup-trace [flags]

Registers the host so watch() can observe filesystem activity. Run once, as
root. A test run never does this for you.

What it writes:
  `+gvisor.TraceConfigPath+`   the gVisor trace session
  `+gvisor.DaemonJSONPath+`    a "`+gvisor.TraceRuntimeName+`" runtime entry (other runtimes untouched)

Flags:
  --check           Report what is installed and what would change. Writes
                    nothing. Exit 1 if registration is missing or stale.
  --with-read       Also trace read/pread64. Off by default: roughly doubles
                    traffic and risks dropped points, which fail tests.
  --with-close      Also trace close, for "opened and never closed".
  --with-connect    Also trace connect.
  --sink PATH       Socket the sandbox reports to (default `+gvisor.DefaultSinkPath+`).

Applying the change needs a Docker daemon restart, which this command prints
rather than performs — it would stop every container on the host.
`)
}
