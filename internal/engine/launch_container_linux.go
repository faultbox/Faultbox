//go:build linux

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

// launchExternal runs the notification loop on a pre-existing seccomp listener fd.
// Used for container mode: the shim inside the container installed the filter and
// reported the listener fd. The Session just needs to run the notification loop.
func (s *Session) launchExternal(ctx context.Context) (*Result, error) {
	start := time.Now()

	listenerFd := s.cfg.ExternalListenerFd
	childPid := s.cfg.ExternalPID

	// Resolve syscall names to numbers for the rule map.
	type resolvedRule struct {
		nr   int32
		rule FaultRule
	}
	var rules []resolvedRule

	for _, r := range s.cfg.FaultRules {
		nr := seccomp.SyscallNumber(r.Syscall)
		if nr < 0 {
			continue
		}
		rules = append(rules, resolvedRule{nr: nr, rule: r})
	}

	// Also intercept "openat" if "open" is requested.
	for _, r := range s.cfg.FaultRules {
		if r.Syscall == "open" {
			openatNr := seccomp.SyscallNumber("openat")
			if openatNr >= 0 {
				rules = append(rules, resolvedRule{nr: openatNr, rule: r})
			}
		}
	}

	// Build rule lookup.
	ruleMap := make(map[int32][]*FaultRule)
	for i := range rules {
		ruleMap[rules[i].nr] = append(ruleMap[rules[i].nr], &rules[i].rule)
	}

	s.setState(StateRunning)
	s.log.Info("external target attached",
		slog.Int("pid", childPid),
		slog.Int("listener_fd", listenerFd),
	)

	// Run the notification loop (same as binary mode).
	stopNotif := make(chan struct{})
	notifDone := make(chan error, 1)
	go func() {
		notifDone <- s.notificationLoop(ctx, listenerFd, ruleMap, stopNotif)
	}()

	// Wait for the process to exit by polling the listener fd.
	// When the target process exits, the listener fd becomes invalid and
	// Poll/Receive will return errors. We also watch /proc/<pid> disappearance.
	waitDone := make(chan struct{})
	var exitCode int
	go func() {
		defer close(waitDone)
		statusPath := fmt.Sprintf("/proc/%d/status", childPid)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Check if process still exists.
			if !processExists(childPid) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Try to get exit status via waitid (non-blocking, we're not the parent).
		// For containers, Docker manages the process — we just detect exit.
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(childPid, &ws, unix.WNOHANG, nil)
		if err == nil && wpid > 0 {
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			}
		}
		// If Wait4 fails (ECHILD — we're not the parent), that's OK for containers.
	}()

	<-waitDone

	// Stop the notification loop.
	close(stopNotif)
	unix.Close(listenerFd)
	<-notifDone
	duration := time.Since(start)

	s.setState(StateStopped)
	s.log.Info("external session completed",
		slog.Int("exit_code", exitCode),
		slog.Duration("duration", duration),
	)

	return &Result{SessionID: s.ID, ExitCode: exitCode, Duration: duration}, nil
}

// processExists checks if a process with the given PID exists.
func processExists(pid int) bool {
	// Signal 0 checks existence without actually sending a signal.
	err := unix.Kill(pid, 0)
	return err == nil
}

