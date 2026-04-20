//go:build linux

// faultbox-shim is a tiny entrypoint binary for container fault injection.
// It is bind-mounted into Docker containers, overriding the entrypoint.
// The shim installs a seccomp-notify filter, sends the listener fd to the
// host via a Unix domain socket (SCM_RIGHTS), then exec's the original
// container entrypoint.
//
// Build: GOOS=linux CGO_ENABLED=0 go build -o faultbox-shim ./cmd/faultbox-shim/
//
// Diagnostics (RFC-022 Phase 0): every handoff phase emits a JSON line to
// stderr. On a hang, the last phase=<name> step=start event identifies
// exactly where we stopped. View with `docker logs <container>`.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
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

// logger emits one JSON line per event to stderr, tagged with "shim" so host
// log readers can filter.
var logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelInfo,
})).With("component", "faultbox-shim")

// phaseStart and phaseDone bracket each handoff step so a hang is identifiable
// by "last start without matching done."
func phaseStart(phase string, attrs ...any) {
	logger.Info("phase", append([]any{"phase", phase, "step", "start"}, attrs...)...)
}

func phaseDone(phase string, attrs ...any) {
	logger.Info("phase", append([]any{"phase", phase, "step", "done"}, attrs...)...)
}

func phaseError(phase string, err error, attrs ...any) {
	logger.Error("phase", append([]any{"phase", phase, "step", "error", "error", err.Error()}, attrs...)...)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "faultbox-shim: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	phaseStart("parse_config")
	configJSON := os.Getenv(envKey)
	if configJSON == "" {
		err := fmt.Errorf("%s env var not set", envKey)
		phaseError("parse_config", err)
		return err
	}

	var cfg ShimConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		phaseError("parse_config", err)
		return fmt.Errorf("parse shim config: %w", err)
	}
	phaseDone("parse_config",
		"syscall_count", len(cfg.SyscallNrs),
		"socket_path", cfg.SocketPath,
		"entrypoint_len", len(cfg.Entrypoint),
		"cmd_len", len(cfg.Cmd),
	)

	// Resolve the binary to exec: entrypoint + cmd combined.
	phaseStart("resolve_binary")
	execArgs := append(cfg.Entrypoint, cfg.Cmd...)
	if len(execArgs) == 0 {
		err := fmt.Errorf("no entrypoint or cmd specified")
		phaseError("resolve_binary", err)
		return err
	}
	// Resolve binary via PATH (unix.Exec requires an absolute path).
	binary, err := exec.LookPath(execArgs[0])
	if err != nil {
		phaseError("resolve_binary", err, "binary", execArgs[0])
		return fmt.Errorf("resolve entrypoint %q: %w", execArgs[0], err)
	}
	phaseDone("resolve_binary", "binary", binary)

	// Lock to OS thread — seccomp filters are per-thread.
	runtime.LockOSThread()

	listenerFd := 0

	if len(cfg.SyscallNrs) > 0 {
		// Required for unprivileged seccomp.
		phaseStart("set_no_new_privs")
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			phaseError("set_no_new_privs", err)
			return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
		}
		phaseDone("set_no_new_privs")

		// Install seccomp filter. Pass fd 2 (stderr) as the whitelist so
		// subsequent phaseDone/phaseStart JSON logs during the critical
		// section between filter install and SCM_RIGHTS send can still
		// write to stderr without being intercepted by our own filter.
		// Without this, our log writes hit the filter, the kernel
		// suspends them waiting for a userspace listener (which hasn't
		// been handed to the host yet) — classic self-deadlock.
		phaseStart("install_filter", "syscall_count", len(cfg.SyscallNrs), "whitelist_fd", 2)
		fd, err := seccomp.InstallFilter(cfg.SyscallNrs, 2)
		if err != nil {
			phaseError("install_filter", err)
			return fmt.Errorf("install seccomp filter: %w", err)
		}
		listenerFd = fd
		phaseDone("install_filter", "listener_fd", fd)
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
		phaseStart("dup3_listener", "from_fd", listenerFd, "to_fd", 255)
		if err := unix.Dup3(listenerFd, 255, 0); err != nil {
			phaseError("dup3_listener", err)
			return fmt.Errorf("dup3 listener fd: %w", err)
		}
		phaseDone("dup3_listener")
	}

	// Build clean environment (remove shim config).
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, envKey+"=") {
			cleanEnv = append(cleanEnv, e)
		}
	}

	// Exec the original entrypoint — seccomp filter survives exec.
	// This is the final phase of the shim — after unix.Exec succeeds, stderr
	// belongs to the real entrypoint and the shim's logger is gone.
	phaseStart("exec", "binary", binary, "argv_len", len(execArgs))
	return unix.Exec(binary, execArgs, cleanEnv)
}

// sendFdViaSocket connects to the host's Unix socket and sends the listener
// fd using SCM_RIGHTS. Waits for a single ACK byte before returning.
// This replaces the pidfd_getfd approach — no PID tracking needed.
func sendFdViaSocket(socketPath string, fd int) error {
	phaseStart("dial_socket", "path", socketPath)
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		phaseError("dial_socket", err, "path", socketPath)
		return fmt.Errorf("connect to %s: %w", socketPath, err)
	}
	defer conn.Close()
	phaseDone("dial_socket")

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		err := fmt.Errorf("not a unix connection")
		phaseError("dial_socket", err)
		return err
	}

	// Send the fd via SCM_RIGHTS ancillary data.
	phaseStart("send_scm", "fd", fd)
	rights := unix.UnixRights(fd)
	_, _, err = uc.WriteMsgUnix([]byte("fd"), rights, nil)
	if err != nil {
		phaseError("send_scm", err)
		return fmt.Errorf("send fd: %w", err)
	}
	phaseDone("send_scm")

	// Wait for host ACK (1 byte).
	phaseStart("recv_ack")
	buf := make([]byte, 1)
	_, err = uc.Read(buf)
	if err != nil {
		phaseError("recv_ack", err)
		return fmt.Errorf("read ack: %w", err)
	}
	phaseDone("recv_ack")

	return nil
}

// legacyReportFd writes the fd number to a file and busy-waits for an ACK file.
// Used when SocketPath is not set (backward compat with older host binaries).
func legacyReportFd(cfg ShimConfig, listenerFd int) error {
	phaseStart("legacy_report_fd", "path", cfg.ReportPath)
	reportFile, err := os.Create(cfg.ReportPath)
	if err != nil {
		phaseError("legacy_report_fd", err)
		return fmt.Errorf("create report file %s: %w", cfg.ReportPath, err)
	}
	fmt.Fprintf(reportFile, "%d\n", listenerFd)
	reportFile.Close()
	phaseDone("legacy_report_fd")

	if cfg.AckPath != "" {
		phaseStart("legacy_wait_ack", "path", cfg.AckPath)
		for {
			if _, err := os.Stat(cfg.AckPath); err == nil {
				break
			}
			ts := unix.Timespec{Nsec: 10_000_000} // 10ms
			unix.Nanosleep(&ts, nil)
		}
		phaseDone("legacy_wait_ack")
	}
	return nil
}
