//go:build linux

// faultbox-shim is a tiny entrypoint binary for container fault injection.
// It is bind-mounted into Docker containers, overriding the entrypoint.
// The shim installs a seccomp-notify filter, writes the listener fd to a
// shared file, then exec's the original container entrypoint.
//
// Build: GOOS=linux CGO_ENABLED=0 go build -o faultbox-shim ./cmd/faultbox-shim/
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

// ShimConfig is passed via the _FAULTBOX_SHIM_CONFIG env var.
type ShimConfig struct {
	SyscallNrs []uint32 `json:"syscall_nrs"`
	Entrypoint []string `json:"entrypoint"` // original container entrypoint
	Cmd        []string `json:"cmd"`        // original container cmd
	ReportPath string   `json:"report_path"` // where to write listener fd (e.g., /var/run/faultbox/listener-fd)
}

const envKey = "_FAULTBOX_SHIM_CONFIG"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "faultbox-shim: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configJSON := os.Getenv(envKey)
	if configJSON == "" {
		return fmt.Errorf("%s env var not set", envKey)
	}

	var cfg ShimConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}

	// Resolve the binary to exec: entrypoint + cmd combined.
	execArgs := append(cfg.Entrypoint, cfg.Cmd...)
	if len(execArgs) == 0 {
		return fmt.Errorf("no entrypoint or cmd specified")
	}
	binary := execArgs[0]

	// Lock to OS thread — seccomp filters are per-thread.
	runtime.LockOSThread()

	// Open the report file BEFORE installing the filter (so we can whitelist this fd).
	reportFile, err := os.Create(cfg.ReportPath)
	if err != nil {
		return fmt.Errorf("create report file %s: %w", cfg.ReportPath, err)
	}
	reportFd := int(reportFile.Fd())

	listenerFd := 0

	if len(cfg.SyscallNrs) > 0 {
		// Required for unprivileged seccomp.
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
		}

		// Install seccomp filter, whitelisting the report fd for writes.
		fd, err := seccomp.InstallFilter(cfg.SyscallNrs, reportFd)
		if err != nil {
			return fmt.Errorf("install seccomp filter: %w", err)
		}
		listenerFd = fd
	}

	// Write the listener fd number to the report file.
	if _, err := reportFile.WriteString(strconv.Itoa(listenerFd) + "\n"); err != nil {
		return fmt.Errorf("write listener fd to report: %w", err)
	}
	reportFile.Close()

	// Build clean environment (remove shim config).
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, envKey+"=") {
			cleanEnv = append(cleanEnv, e)
		}
	}

	// Exec the original entrypoint — seccomp filter survives exec.
	return unix.Exec(binary, execArgs, cleanEnv)
}
