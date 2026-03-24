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
	// SyscallNrs to intercept.
	SyscallNrs []uint32 `json:"syscall_nrs"`
	// TargetBinary to exec after installing the filter.
	TargetBinary string `json:"target_binary"`
	// TargetArgs for the target binary.
	TargetArgs []string `json:"target_args"`
	// PipeFd is the write end of a pipe for signaling the parent.
	PipeFd int `json:"pipe_fd"`
}

// IsShimChild returns true if this process is a re-exec'd shim child.
func IsShimChild() bool {
	return os.Getenv(ShimEnvKey) != ""
}

// RunShimChild is called in the child process. It:
// 1. Installs the seccomp filter
// 2. Writes the listener fd number to the parent via pipe
// 3. Execs the target binary (filter survives exec)
func RunShimChild() error {
	configJSON := os.Getenv(ShimEnvKey)
	var cfg ShimConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("parse shim config: %w", err)
	}

	// Lock to OS thread — seccomp filters are per-thread, and we need
	// the filter on the thread that will call exec().
	runtime.LockOSThread()

	// Required before installing a seccomp filter — tells the kernel
	// this process won't gain new privileges (needed for unprivileged seccomp).
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}

	// Install the seccomp filter — this process gets filtered.
	// Pass the pipe fd so write() to it is allowed (avoids deadlock).
	listenerFd, err := InstallFilter(cfg.SyscallNrs, cfg.PipeFd)
	if err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}

	// Signal the parent: write the listener fd number to the pipe.
	msg := strconv.Itoa(listenerFd) + "\n"
	if _, err := unix.Write(cfg.PipeFd, []byte(msg)); err != nil {
		return fmt.Errorf("write listener fd to pipe: %w", err)
	}
	unix.Close(cfg.PipeFd)

	// Exec the target binary — the seccomp filter survives exec().
	// This replaces the current process.
	return unix.Exec(cfg.TargetBinary, append([]string{cfg.TargetBinary}, cfg.TargetArgs...), os.Environ())
}

// StartWithFilter launches the current binary as a child shim that installs
// a seccomp filter, then execs the target. Returns the child PID and the
// listener fd (in this process's fd table).
//
// Flow:
//  1. Parent creates a pipe
//  2. Parent forks itself with _FAULTBOX_SECCOMP_CHILD env
//  3. Child installs filter, writes listener fd number to pipe, execs target
//  4. Parent reads fd number from pipe
//  5. Parent uses pidfd_getfd() to copy the listener fd from child
func StartWithFilter(targetBinary string, targetArgs []string, syscallNrs []uint32, extraFiles []*os.File) (pid int, listenerFd int, err error) {
	// Create a pipe for child → parent communication.
	pipeFds := [2]int{}
	if err := unix.Pipe2(pipeFds[:], unix.O_CLOEXEC); err != nil {
		return 0, -1, fmt.Errorf("pipe2: %w", err)
	}
	pipeR, pipeW := pipeFds[0], pipeFds[1]
	defer unix.Close(pipeR)

	// The child needs the write end of the pipe. We pass it as an extra fd.
	// Find the fd number it will get in the child (after stdin/stdout/stderr + extraFiles).
	childPipeFd := 3 + len(extraFiles)

	cfg := ShimConfig{
		SyscallNrs:   syscallNrs,
		TargetBinary: targetBinary,
		TargetArgs:   targetArgs,
		PipeFd:       childPipeFd,
	}
	cfgJSON, err := json.Marshal(cfg)
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

	// Build file descriptors: stdin, stdout, stderr, [extraFiles...], pipeW
	fds := []uintptr{0, 1, 2}
	for _, f := range extraFiles {
		fds = append(fds, f.Fd())
	}
	fds = append(fds, uintptr(pipeW))

	// Build environment with shim config.
	env := append(os.Environ(), ShimEnvKey+"="+string(cfgJSON))

	// Fork+exec ourselves as the child shim.
	childPid, err := syscall.ForkExec(self, os.Args[:1], &syscall.ProcAttr{
		Env:   env,
		Files: fds,
	})
	if err != nil {
		unix.Close(pipeW)
		return 0, -1, fmt.Errorf("forkexec shim: %w", err)
	}

	// Parent: close write end of pipe, read child's listener fd number.
	unix.Close(pipeW)

	buf := make([]byte, 32)
	n, err := unix.Read(pipeR, buf)
	if err != nil {
		return childPid, -1, fmt.Errorf("read listener fd from child: %w", err)
	}
	if n == 0 {
		return childPid, -1, fmt.Errorf("child shim exited before sending listener fd (check child stderr)")
	}

	fdStr := strings.TrimSpace(string(buf[:n]))
	childListenerFd, err := strconv.Atoi(fdStr)
	if err != nil {
		return childPid, -1, fmt.Errorf("parse listener fd %q: %w", fdStr, err)
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
