//go:build linux

package seccomp

import (
	"encoding/json"
	"fmt"
	"os"
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
}


// IsShimChild returns true if this process is a re-exec'd shim child.
func IsShimChild() bool {
	return os.Getenv(ShimEnvKey) != ""
}

// RunShimChild is called in the child process. It:
// 1. Optionally installs the seccomp filter
// 2. Writes the listener fd (or "0" if no filter) to the parent via pipe
// 3. Execs the target binary
func RunShimChild() error {
	configJSON := os.Getenv(ShimEnvKey)
	var cfg ShimConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
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
	// This replaces the current process.
	return unix.Exec(cfg.TargetBinary, append([]string{cfg.TargetBinary}, cfg.TargetArgs...), cleanEnv)
}

// Launch starts the target binary via the re-exec shim pattern.
// The child is created with the specified clone flags (namespaces) and
// optionally installs a seccomp filter before exec'ing the target.
//
// Returns the child PID and the listener fd (or -1 if no filter).
//
// Flow:
//  1. Parent creates a pipe
//  2. Parent ForkExecs itself with clone flags + _FAULTBOX_SECCOMP_CHILD env
//  3. Child (in new namespaces) optionally installs filter, writes fd to pipe, execs target
//  4. Parent reads fd from pipe
//  5. If filter was installed, parent uses pidfd_getfd() to copy the listener fd
func Launch(cfg LaunchConfig) (pid int, listenerFd int, err error) {
	// Create a pipe for child → parent communication.
	pipeFds := [2]int{}
	if err := unix.Pipe2(pipeFds[:], unix.O_CLOEXEC); err != nil {
		return 0, -1, fmt.Errorf("pipe2: %w", err)
	}
	pipeR, pipeW := pipeFds[0], pipeFds[1]
	defer unix.Close(pipeR)

	// The child gets: stdin(0), stdout(1), stderr(2), pipeW(3).
	childPipeFd := 3

	shimCfg := ShimConfig{
		SyscallNrs:   cfg.SyscallNrs,
		TargetBinary: cfg.TargetBinary,
		TargetArgs:   cfg.TargetArgs,
		TargetEnv:    cfg.TargetEnv,
		PipeFd:       childPipeFd,
	}
	cfgJSON, err := json.Marshal(shimCfg)
	if err != nil {
		unix.Close(pipeW)
		return 0, -1, fmt.Errorf("marshal shim config: %w", err)
	}

	// Get our own executable path for re-exec.
	self, err := os.Executable()
	if err != nil {
		unix.Close(pipeW)
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
		Env:   env,
		Files: fds,
		Sys:   sysAttr,
	})
	if err != nil {
		unix.Close(pipeW)
		return 0, -1, fmt.Errorf("forkexec shim: %w", err)
	}

	// Parent: close write end of pipe, read child's message.
	unix.Close(pipeW)

	buf := make([]byte, 32)
	n, err := unix.Read(pipeR, buf)
	if err != nil {
		return childPid, -1, fmt.Errorf("read from child shim: %w", err)
	}
	if n == 0 {
		return childPid, -1, fmt.Errorf("child shim exited before signaling (check child stderr)")
	}

	fdStr := strings.TrimSpace(string(buf[:n]))
	childListenerFd, err := strconv.Atoi(fdStr)
	if err != nil {
		return childPid, -1, fmt.Errorf("parse listener fd %q: %w", fdStr, err)
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
