//go:build linux

package seccomp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ShimEnvKey is set when faultbox re-execs itself as the child shim.
const ShimEnvKey = "_FAULTBOX_SECCOMP_CHILD"

// ShimConfig is passed from parent to child via the shim env var.
type ShimConfig struct {
	// SyscallNrs to intercept (empty = no seccomp filter).
	SyscallNrs []uint32 `json:"syscall_nrs,omitempty"`
	// TargetBinary to exec after setup.
	TargetBinary string `json:"target_binary"`
	// TargetArgs for the target binary.
	TargetArgs []string `json:"target_args"`
	// TargetEnv is the environment for the target (replaces inherited env).
	TargetEnv []string `json:"target_env,omitempty"`
	// PipeFd is the write end of a pipe for signaling the parent.
	PipeFd int `json:"pipe_fd"`
	// GateFd is the optional parent-to-child permit pipe (0 means ungated).
	GateFd int `json:"gate_fd,omitempty"`
}

// IsShimChild returns true if this process is a re-exec'd shim child.
func IsShimChild() bool {
	return os.Getenv(ShimEnvKey) != ""
}

// RunShimChild is called in the child process. It:
// 1. Waits for the optional parent registration permit
// 2. Resolves and verifies the target binary
// 3. Optionally installs the seccomp filter
// 4. Writes the listener fd (or "0" if no filter) to the parent via pipe
// 5. Execs the target binary
func RunShimChild() error {
	configJSON := os.Getenv(ShimEnvKey)
	var cfg ShimConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}

	if cfg.GateFd > 0 {
		if err := awaitExecPermit(cfg.GateFd); err != nil {
			writePipeError(cfg.PipeFd, err)
			return err
		}
	}

	// Resolve and verify the target BEFORE signaling the parent. The
	// exec happens after the pipe write, so without this check a
	// missing binary used to surface as exit_code=0 + a healthcheck
	// timeout 60s later instead of an immediate, named error
	// (F-2, v0.13.0 eval). LookPath also covers bare names — unix.Exec
	// does not search PATH on its own.
	binary, err := exec.LookPath(cfg.TargetBinary)
	if err != nil {
		err = fmt.Errorf("exec %s: %w", cfg.TargetBinary, err)
		writePipeError(cfg.PipeFd, err)
		return err
	}

	// Lock to OS thread — seccomp filters are per-thread, and we need
	// the filter on the thread that will call exec().
	runtime.LockOSThread()

	listenerFd := 0 // 0 means no filter

	if len(cfg.SyscallNrs) > 0 {
		// Required before installing a seccomp filter — tells the kernel
		// this process won't gain new privileges (needed for unprivileged seccomp).
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
		}

		// Install the seccomp filter — this process gets filtered.
		// Pass the pipe fd so write() to it is allowed (avoids deadlock).
		// No socket-fd whitelist needed in legacy binary-mode path
		// (-1 disables the socket-family exceptions added in RFC-022 v0.9.1).
		fd, err := InstallFilter(cfg.SyscallNrs, cfg.PipeFd, -1)
		if err != nil {
			return fmt.Errorf("install seccomp filter: %w", err)
		}
		listenerFd = fd
	}

	// Signal the parent: write the listener fd number (or "0") to the pipe.
	msg := strconv.Itoa(listenerFd) + "\n"
	if _, err := unix.Write(cfg.PipeFd, []byte(msg)); err != nil {
		return fmt.Errorf("write listener fd to pipe: %w", err)
	}
	unix.Close(cfg.PipeFd)

	// Build environment for the target.
	env := cfg.TargetEnv
	if len(env) == 0 {
		env = os.Environ()
	}
	// Remove the shim env var so the target doesn't see it.
	cleanEnv := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, ShimEnvKey+"=") {
			cleanEnv = append(cleanEnv, e)
		}
	}

	// Exec the target binary — the seccomp filter (if any) survives exec().
	// This replaces the current process; unix.Exec only returns on failure.
	err = unix.Exec(binary, append([]string{cfg.TargetBinary}, cfg.TargetArgs...), cleanEnv)
	return fmt.Errorf("exec %s: %w", binary, err)
}

// awaitExecPermit accepts exactly one explicit permit byte. EOF, a closed
// descriptor or a malformed permit must never allow the target to execute.
func awaitExecPermit(fd int) error {
	defer unix.Close(fd)
	var permit [1]byte
	for {
		n, err := unix.Read(fd, permit[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for parent exec permit: %w", err)
		}
		if n != 1 || permit[0] != 1 {
			return fmt.Errorf("parent exec gate closed without a valid permit")
		}
		return nil
	}
}

// writePipeError sends an "ERR <message>" line to the parent over the
// signaling pipe so launch fails immediately with the root cause
// instead of a downstream healthcheck timeout. Best-effort: if the
// write fails the parent still sees pipe EOF and reports a generic
// child-exit error.
func writePipeError(pipeFd int, err error) {
	msg := "ERR " + strings.ReplaceAll(err.Error(), "\n", " ") + "\n"
	unix.Write(pipeFd, []byte(msg))
	unix.Close(pipeFd)
}

// parseShimSignal interprets the child's pipe message. The protocol
// has two shapes:
//
//	"<fd>\n"          — setup succeeded; fd is the seccomp listener
//	                    (0 = no filter installed)
//	"ERR <message>\n" — the child failed before exec; message is the
//	                    root cause (e.g. "exec /tmp/svc: no such file
//	                    or directory")
//
// Split out of Launch so the protocol is unit-testable without a
// fork+re-exec round trip.
func parseShimSignal(raw string) (listenerFd int, childErr error) {
	s := strings.TrimSpace(raw)
	if msg, ok := strings.CutPrefix(s, "ERR "); ok {
		return -1, fmt.Errorf("launch target: %s", msg)
	}
	fd, err := strconv.Atoi(s)
	if err != nil {
		return -1, fmt.Errorf("parse listener fd %q: %w", s, err)
	}
	return fd, nil
}

// Launch starts the target binary via the re-exec shim pattern.
// The child is created with the specified clone flags (namespaces) and
// optionally installs a seccomp filter before exec'ing the target.
//
// Returns the child PID and the listener fd (or -1 if no filter).
//
// Flow:
//  1. Parent creates the signal pipe and, when requested, an exec gate
//  2. Parent ForkExecs itself with clone flags + _FAULTBOX_SECCOMP_CHILD env
//  3. Parent calls BeforeExec with the host PID, then releases the child gate
//  4. Child (in new namespaces) optionally installs filter, writes fd to pipe, execs target
//  5. Parent reads fd from pipe
//  6. If filter was installed, parent uses pidfd_getfd() to copy the listener fd
func Launch(cfg LaunchConfig) (pid int, listenerFd int, err error) {
	// Create a pipe for child → parent communication.
	pipeFds := [2]int{}
	if err := unix.Pipe2(pipeFds[:], unix.O_CLOEXEC); err != nil {
		return 0, -1, fmt.Errorf("pipe2: %w", err)
	}
	pipeR, pipeW := pipeFds[0], pipeFds[1]
	defer unix.Close(pipeR)
	defer func() {
		if pipeW >= 0 {
			unix.Close(pipeW)
		}
	}()

	gateR, gateW := -1, -1
	if cfg.BeforeExec != nil {
		gate := [2]int{}
		if err := unix.Pipe2(gate[:], unix.O_CLOEXEC); err != nil {
			return 0, -1, fmt.Errorf("exec gate pipe2: %w", err)
		}
		gateR, gateW = gate[0], gate[1]
	}
	defer func() {
		if gateR >= 0 {
			unix.Close(gateR)
		}
		if gateW >= 0 {
			unix.Close(gateW)
		}
	}()

	// The child gets: stdin(0), stdout(1), stderr(2), pipeW(3).
	childPipeFd := 3

	shimCfg := ShimConfig{
		SyscallNrs:   cfg.SyscallNrs,
		TargetBinary: cfg.TargetBinary,
		TargetArgs:   cfg.TargetArgs,
		TargetEnv:    cfg.TargetEnv,
		PipeFd:       childPipeFd,
	}
	if cfg.BeforeExec != nil {
		shimCfg.GateFd = 4
	}
	cfgJSON, err := json.Marshal(shimCfg)
	if err != nil {
		return 0, -1, fmt.Errorf("marshal shim config: %w", err)
	}

	// Get our own executable path for re-exec.
	self, err := os.Executable()
	if err != nil {
		return 0, -1, fmt.Errorf("get executable path: %w", err)
	}

	// Build file descriptors: stdin, stdout, stderr, pipeW.
	// StdoutFd/StderrFd override stdout/stderr if set (for --format json).
	stdoutFd := uintptr(1)
	stderrFd := uintptr(2)
	if cfg.StdoutFd != 0 {
		stdoutFd = cfg.StdoutFd
	}
	if cfg.StderrFd != 0 {
		stderrFd = cfg.StderrFd
	}
	fds := []uintptr{0, stdoutFd, stderrFd, uintptr(pipeW)}
	if gateR >= 0 {
		fds = append(fds, uintptr(gateR))
	}

	// Build environment with shim config.
	env := append(os.Environ(), ShimEnvKey+"="+string(cfgJSON))

	// Build SysProcAttr with optional clone flags.
	sysAttr := &syscall.SysProcAttr{}
	if cfg.Cloneflags != 0 {
		sysAttr.Cloneflags = cfg.Cloneflags
		for _, m := range cfg.UidMappings {
			sysAttr.UidMappings = append(sysAttr.UidMappings, syscall.SysProcIDMap{
				ContainerID: m.ContainerID, HostID: m.HostID, Size: m.Size,
			})
		}
		for _, m := range cfg.GidMappings {
			sysAttr.GidMappings = append(sysAttr.GidMappings, syscall.SysProcIDMap{
				ContainerID: m.ContainerID, HostID: m.HostID, Size: m.Size,
			})
		}
	}

	// Fork+exec ourselves as the child shim.
	childPid, err := syscall.ForkExec(self, os.Args[:1], &syscall.ProcAttr{
		Dir:   cfg.TargetDir,
		Env:   env,
		Files: fds,
		Sys:   sysAttr,
	})
	if err != nil {
		return 0, -1, fmt.Errorf("forkexec shim: %w", err)
	}

	// Every post-fork launch failure must leave no blocked child or zombie.
	// The PID cannot be reused before this parent's wait, so kill is safe here.
	defer func() {
		if err != nil {
			_ = unix.Kill(childPid, unix.SIGKILL)
			for {
				_, waitErr := unix.Wait4(childPid, nil, 0, nil)
				if waitErr != unix.EINTR {
					break
				}
			}
		}
	}()
	unix.Close(pipeW)
	pipeW = -1
	if gateR >= 0 {
		unix.Close(gateR)
		gateR = -1
		if callbackErr := cfg.BeforeExec(childPid); callbackErr != nil {
			return childPid, -1, fmt.Errorf("register process before exec: %w", callbackErr)
		}
		for {
			n, writeErr := unix.Write(gateW, []byte{1})
			if writeErr == unix.EINTR {
				continue
			}
			if writeErr != nil {
				return childPid, -1, fmt.Errorf("release child exec gate: %w", writeErr)
			}
			if n != 1 {
				return childPid, -1, fmt.Errorf("release child exec gate: short write")
			}
			break
		}
		unix.Close(gateW)
		gateW = -1
	}

	// Read the setup signal after registration has released the child.

	buf := make([]byte, 512)
	n, err := unix.Read(pipeR, buf)
	if err != nil {
		return childPid, -1, fmt.Errorf("read from child shim: %w", err)
	}
	if n == 0 {
		return childPid, -1, fmt.Errorf("child shim exited before signaling (check child stderr)")
	}

	childListenerFd, childErr := parseShimSignal(string(buf[:n]))
	if childErr != nil {
		// The child failed before exec (e.g. target binary missing)
		// and reported the root cause over the pipe. Reap it and fail
		// the launch immediately with that message (F-2).
		return childPid, -1, childErr
	}

	// "0" means no seccomp filter was installed.
	if childListenerFd == 0 {
		return childPid, -1, nil
	}

	// Use pidfd_getfd() to copy the listener fd from the child's fd table.
	pidfd, err := unix.PidfdOpen(childPid, 0)
	if err != nil {
		return childPid, -1, fmt.Errorf("pidfd_open(%d): %w", childPid, err)
	}
	defer unix.Close(pidfd)

	localFd, err := unix.PidfdGetfd(pidfd, childListenerFd, 0)
	if err != nil {
		return childPid, -1, fmt.Errorf("pidfd_getfd(fd=%d): %w", childListenerFd, err)
	}

	return childPid, localFd, nil
}

// StartWithFilter is the legacy API — launches with seccomp filter but no namespaces.
// Deprecated: use Launch instead.
func StartWithFilter(targetBinary string, targetArgs []string, syscallNrs []uint32, extraFiles []*os.File) (pid int, listenerFd int, err error) {
	return Launch(LaunchConfig{
		TargetBinary: targetBinary,
		TargetArgs:   targetArgs,
		SyscallNrs:   syscallNrs,
	})
}
