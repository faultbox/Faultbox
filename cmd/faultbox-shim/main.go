//go:build linux

// faultbox-shim is a tiny entrypoint binary for container fault injection.
// It is bind-mounted into Docker containers, overriding the entrypoint.
// The shim installs a seccomp-notify filter, sends the listener fd to the
// host via a Unix domain socket (SCM_RIGHTS), then exec's the original
// container entrypoint.
//
// Build: GOOS=linux CGO_ENABLED=0 go build -o faultbox-shim ./cmd/faultbox-shim/
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

// ShimConfig is passed via the _FAULTBOX_SHIM_CONFIG env var.
type ShimConfig struct {
	SyscallNrs []uint32 `json:"syscall_nrs"`
	Entrypoint []string `json:"entrypoint"`   // original container entrypoint
	Cmd        []string `json:"cmd"`          // original container cmd
	SocketPath string   `json:"socket_path"`  // Unix socket for fd passing to host

	// Legacy fields (ignored if SocketPath is set).
	ReportPath string `json:"report_path"`
	AckPath    string `json:"ack_path"`
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
	// Resolve binary via PATH (unix.Exec requires an absolute path).
	binary, err := exec.LookPath(execArgs[0])
	if err != nil {
		return fmt.Errorf("resolve entrypoint %q: %w", execArgs[0], err)
	}

	// Lock to OS thread — seccomp filters are per-thread.
	runtime.LockOSThread()

	listenerFd := 0

	if len(cfg.SyscallNrs) > 0 {
		// Required for unprivileged seccomp.
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
		}

		// Install seccomp filter. Pass -1 as whitelist fd (no file-based reporting).
		fd, err := seccomp.InstallFilter(cfg.SyscallNrs, -1)
		if err != nil {
			return fmt.Errorf("install seccomp filter: %w", err)
		}
		listenerFd = fd
	}

	// Send the listener fd to the host via Unix domain socket (SCM_RIGHTS).
	if cfg.SocketPath != "" {
		if err := sendFdViaSocket(cfg.SocketPath, listenerFd); err != nil {
			return fmt.Errorf("send fd via socket: %w", err)
		}
	} else if cfg.ReportPath != "" {
		// Legacy fallback: file-based reporting + busy-wait ACK.
		if err := legacyReportFd(cfg, listenerFd); err != nil {
			return err
		}
	}

	// Keep the listener fd open across exec — the kernel's seccomp listener
	// refcount must stay > 0. Dup to fd 255 (shells won't close it).
	if listenerFd > 0 {
		unix.Dup3(listenerFd, 255, 0)
	}

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

// sendFdViaSocket connects to the host's Unix socket and sends the listener
// fd using SCM_RIGHTS. Waits for a single ACK byte before returning.
// This replaces the pidfd_getfd approach — no PID tracking needed.
func sendFdViaSocket(socketPath string, fd int) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", socketPath, err)
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}

	// Send the fd via SCM_RIGHTS ancillary data.
	rights := unix.UnixRights(fd)
	_, _, err = uc.WriteMsgUnix([]byte("fd"), rights, nil)
	if err != nil {
		return fmt.Errorf("send fd: %w", err)
	}

	// Wait for host ACK (1 byte).
	buf := make([]byte, 1)
	_, err = uc.Read(buf)
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}

	return nil
}

// legacyReportFd writes the fd number to a file and busy-waits for an ACK file.
// Used when SocketPath is not set (backward compat with older host binaries).
func legacyReportFd(cfg ShimConfig, listenerFd int) error {
	reportFile, err := os.Create(cfg.ReportPath)
	if err != nil {
		return fmt.Errorf("create report file %s: %w", cfg.ReportPath, err)
	}
	fmt.Fprintf(reportFile, "%d\n", listenerFd)
	reportFile.Close()

	if cfg.AckPath != "" {
		for {
			if _, err := os.Stat(cfg.AckPath); err == nil {
				break
			}
			ts := unix.Timespec{Nsec: 10_000_000} // 10ms
			unix.Nanosleep(&ts, nil)
		}
	}
	return nil
}
