//go:build linux

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

func (s *Session) runWithFaults(ctx context.Context) (*Result, error) {
	return s.runWithSeccomp(ctx)
}

// runWithSeccomp launches the target via the re-exec shim and runs the
// seccomp notification loop to intercept and fault-inject syscalls.
func (s *Session) runWithSeccomp(ctx context.Context) (*Result, error) {
	start := time.Now()

	// Resolve syscall names to numbers.
	type resolvedRule struct {
		nr   int32
		rule FaultRule
	}
	var rules []resolvedRule
	var syscallNrs []uint32
	seen := make(map[int32]bool)

	for _, r := range s.cfg.FaultRules {
		nr := seccomp.SyscallNumber(r.Syscall)
		if nr < 0 {
			return nil, fmt.Errorf("unknown syscall %q", r.Syscall)
		}
		rules = append(rules, resolvedRule{nr: nr, rule: r})
		if !seen[nr] {
			syscallNrs = append(syscallNrs, uint32(nr))
			seen[nr] = true
		}
	}

	// Also intercept "openat" if "open" is requested (Go uses openat).
	for _, r := range s.cfg.FaultRules {
		if r.Syscall == "open" {
			openatNr := seccomp.SyscallNumber("openat")
			if openatNr >= 0 && !seen[openatNr] {
				syscallNrs = append(syscallNrs, uint32(openatNr))
				seen[openatNr] = true
				rules = append(rules, resolvedRule{nr: openatNr, rule: r})
			}
		}
	}

	s.log.Info("starting seccomp-intercepted session",
		slog.String("binary", s.cfg.Binary),
		slog.Any("syscalls", syscallNrs),
		slog.Int("rule_count", len(rules)),
	)
	s.setState(StateStarting)

	// Launch via re-exec shim.
	childPid, listenerFd, err := seccomp.StartWithFilter(
		s.cfg.Binary, s.cfg.Args, syscallNrs, nil,
	)
	if err != nil {
		s.setState(StateFailed)
		return &Result{SessionID: s.ID, ExitCode: -1, Duration: time.Since(start), Error: err}, err
	}
	s.setState(StateRunning)
	s.log.Info("target started with seccomp filter",
		slog.Int("pid", childPid),
		slog.Int("listener_fd", listenerFd),
	)

	// Build a lookup: syscall nr → applicable rules
	ruleMap := make(map[int32][]FaultRule)
	for _, r := range rules {
		ruleMap[r.nr] = append(ruleMap[r.nr], r.rule)
	}

	// stopNotif signals the notification loop to exit.
	stopNotif := make(chan struct{})

	// Run notification loop in a goroutine.
	notifDone := make(chan error, 1)
	go func() {
		notifDone <- s.notificationLoop(ctx, listenerFd, ruleMap, stopNotif)
	}()

	// Wait for the child. Poll /proc/<pid>/status for zombie or disappeared state.
	// We can't reliably use Wait4 because Go's runtime may reap the child via
	// its SIGCHLD handler before we get to call Wait4.
	waitDone := make(chan struct{})
	var waitErr error
	exitCode := 0
	go func() {
		statusPath := fmt.Sprintf("/proc/%d/status", childPid)
		for {
			data, err := os.ReadFile(statusPath)
			if os.IsNotExist(err) {
				// Process fully gone (already reaped).
				break
			}
			if err == nil {
				// Check for zombie state.
				s := string(data)
				if strings.Contains(s, "State:\tZ") {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Try to reap — may get ECHILD if Go runtime already reaped.
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(childPid, &ws, unix.WNOHANG, nil)
		if err == nil && wpid > 0 {
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			}
		}
		// ECHILD is fine — means already reaped.
		if err != nil && err != unix.ECHILD {
			waitErr = err
		}
		close(waitDone)
	}()

	<-waitDone

	// Signal the notification loop to stop and close the fd.
	close(stopNotif)
	unix.Close(listenerFd)
	<-notifDone
	duration := time.Since(start)

	err = waitErr

	if err != nil {
		s.setState(StateFailed)
		return &Result{SessionID: s.ID, ExitCode: -1, Duration: duration, Error: err}, err
	}

	if exitCode != 0 {
		s.log.Warn("target exited with error",
			slog.Int("exit_code", exitCode),
			slog.Duration("duration", duration),
		)
	}

	s.setState(StateStopped)
	s.log.Info("session completed",
		slog.Int("exit_code", exitCode),
		slog.Duration("duration", duration),
	)

	return &Result{SessionID: s.ID, ExitCode: exitCode, Duration: duration}, nil
}

func (s *Session) notificationLoop(ctx context.Context, listenerFd int, ruleMap map[int32][]FaultRule, stop <-chan struct{}) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stop:
			return nil
		default:
		}

		// Poll the listener fd with a short timeout so we can check the stop channel.
		ready, err := seccomp.Poll(listenerFd, 10) // 10ms timeout
		if err != nil {
			if isClosedFdErr(err) {
				return nil
			}
			return fmt.Errorf("poll listener: %w", err)
		}
		if !ready {
			continue // timeout — check stop channel again
		}

		req, err := seccomp.Receive(listenerFd)
		if err != nil {
			if isClosedFdErr(err) {
				return nil
			}
			return fmt.Errorf("receive notification: %w", err)
		}

		syscallName := seccomp.SyscallName(req.Data.Nr)
		decision := "allow"

		// Check fault rules for this syscall.
		if rules, ok := ruleMap[req.Data.Nr]; ok {
			for _, rule := range rules {
				if rand.Float64() < rule.Probability {
					decision = fmt.Sprintf("deny(%s)", rule.Errno)

					s.log.Info("syscall intercepted",
						slog.String("name", syscallName),
						slog.Int("pid", int(req.PID)),
						slog.String("decision", decision),
						slog.String("errno", rule.Errno.Error()),
					)

					if err := seccomp.Deny(listenerFd, req.ID, int32(rule.Errno)); err != nil {
						if isClosedFdErr(err) {
							return nil
						}
						s.log.Error("failed to deny syscall", slog.String("error", err.Error()))
					}
					goto nextNotif
				}
			}
		}

		// Allow: let the kernel handle the syscall.
		s.log.Debug("syscall intercepted",
			slog.String("name", syscallName),
			slog.Int("pid", int(req.PID)),
			slog.String("decision", decision),
		)

		if err := seccomp.Allow(listenerFd, req.ID); err != nil {
			if isClosedFdErr(err) {
				return nil
			}
			s.log.Error("failed to allow syscall", slog.String("error", err.Error()))
		}

	nextNotif:
	}
}

func isClosedFdErr(err error) bool {
	if err == nil {
		return false
	}
	// Check for common "fd closed" errors.
	if os.IsNotExist(err) {
		return true
	}
	errStr := err.Error()
	return contains(errStr, "bad file descriptor") ||
		contains(errStr, string(syscall.EBADF.Error())) ||
		contains(errStr, string(syscall.ENOENT.Error()))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
