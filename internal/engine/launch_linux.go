//go:build linux

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

func (s *Session) launch(ctx context.Context) (*Result, error) {
	start := time.Now()

	// Resolve syscall names to numbers for the seccomp filter.
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

	// Build clone flags from namespace config.
	ns := s.cfg.Namespaces
	var cloneflags uintptr
	if ns.PID {
		cloneflags |= syscall.CLONE_NEWPID
		s.log.Info("namespace enabled", slog.String("type", "PID"))
	}
	if ns.Network {
		cloneflags |= syscall.CLONE_NEWNET
		s.log.Info("namespace enabled", slog.String("type", "NET"))
	}
	if ns.Mount {
		cloneflags |= syscall.CLONE_NEWNS
		s.log.Info("namespace enabled", slog.String("type", "MNT"))
	}
	if ns.User {
		cloneflags |= syscall.CLONE_NEWUSER
		s.log.Info("namespace enabled", slog.String("type", "USER"))
	}

	// Build uid/gid mappings for user namespace.
	var uidMappings, gidMappings []seccomp.IDMapping
	if ns.User {
		uidMappings = []seccomp.IDMapping{
			{ContainerID: 0, HostID: syscall.Getuid(), Size: 1},
		}
		gidMappings = []seccomp.IDMapping{
			{ContainerID: 0, HostID: syscall.Getgid(), Size: 1},
		}
		s.log.Debug("user namespace mapping",
			slog.Int("host_uid", syscall.Getuid()),
			slog.Int("host_gid", syscall.Getgid()),
		)
	}

	// Build target environment: inherit current + extra env vars.
	var targetEnv []string
	if len(s.cfg.Env) > 0 {
		targetEnv = append(os.Environ(), s.cfg.Env...)
	}

	hasFaults := len(syscallNrs) > 0

	s.log.Info("starting session",
		slog.String("binary", s.cfg.Binary),
		slog.Any("args", s.cfg.Args),
		slog.Int("rule_count", len(rules)),
		slog.Bool("namespaces", cloneflags != 0),
		slog.Bool("seccomp", hasFaults),
	)
	s.setState(StateStarting)

	if hasFaults {
		s.log.Info("seccomp filter installed",
			slog.Any("syscalls", syscallNrs),
		)
	}

	// Launch via unified shim.
	childPid, listenerFd, err := seccomp.Launch(seccomp.LaunchConfig{
		TargetBinary: s.cfg.Binary,
		TargetArgs:   s.cfg.Args,
		TargetEnv:    targetEnv,
		SyscallNrs:   syscallNrs,
		Cloneflags:   cloneflags,
		UidMappings:  uidMappings,
		GidMappings:  gidMappings,
	})
	if err != nil {
		s.setState(StateFailed)
		return &Result{SessionID: s.ID, ExitCode: -1, Duration: time.Since(start), Error: err}, err
	}
	s.setState(StateRunning)

	logFields := []any{slog.Int("pid", childPid)}
	if listenerFd >= 0 {
		logFields = append(logFields, slog.Int("listener_fd", listenerFd))
	}
	s.log.Info("target started", logFields...)

	// Build rule lookup: syscall nr → applicable rules (pointers for atomic counters).
	ruleMap := make(map[int32][]*FaultRule)
	for i := range rules {
		ruleMap[rules[i].nr] = append(ruleMap[rules[i].nr], &rules[i].rule)
	}

	// If we have a seccomp listener, run the notification loop.
	stopNotif := make(chan struct{})
	notifDone := make(chan error, 1)
	if listenerFd >= 0 {
		go func() {
			notifDone <- s.notificationLoop(ctx, listenerFd, ruleMap, stopNotif)
		}()
	} else {
		close(notifDone)
	}

	// Kill child when context is cancelled.
	go func() {
		<-ctx.Done()
		// Send SIGTERM first, then SIGKILL after 2s.
		unix.Kill(childPid, unix.SIGTERM)
		time.Sleep(2 * time.Second)
		unix.Kill(childPid, unix.SIGKILL)
	}()

	// Wait for the child process.
	exitCode := 0
	var waitErr error
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		statusPath := fmt.Sprintf("/proc/%d/status", childPid)
		for {
			data, err := os.ReadFile(statusPath)
			if os.IsNotExist(err) {
				break
			}
			if err == nil {
				if strings.Contains(string(data), "State:\tZ") {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(childPid, &ws, unix.WNOHANG, nil)
		if err == nil && wpid > 0 {
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			}
		}
		if err != nil && err != unix.ECHILD {
			waitErr = err
		}
	}()

	<-waitDone

	// Stop the notification loop and close the fd.
	close(stopNotif)
	if listenerFd >= 0 {
		unix.Close(listenerFd)
	}
	<-notifDone
	duration := time.Since(start)

	if waitErr != nil {
		s.setState(StateFailed)
		return &Result{SessionID: s.ID, ExitCode: -1, Duration: duration, Error: waitErr}, waitErr
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

func (s *Session) notificationLoop(ctx context.Context, listenerFd int, ruleMap map[int32][]*FaultRule, stop <-chan struct{}) error {
	// WaitGroup tracks in-flight notification handlers (needed for delays).
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stop:
			return nil
		default:
		}

		ready, err := seccomp.Poll(listenerFd, 10)
		if err != nil {
			if isClosedFdErr(err) {
				return nil
			}
			return fmt.Errorf("poll listener: %w", err)
		}
		if !ready {
			continue
		}

		req, err := seccomp.Receive(listenerFd)
		if err != nil {
			if isClosedFdErr(err) {
				return nil
			}
			return fmt.Errorf("receive notification: %w", err)
		}

		// Handle each notification in its own goroutine so that delay
		// rules don't block processing of other intercepted syscalls.
		wg.Add(1)
		go func(req *seccomp.NotifReq) {
			defer wg.Done()
			s.handleNotification(listenerFd, req, ruleMap)
		}(req)
	}
}

// handleNotification processes a single seccomp notification: reads path args,
// checks fault rules, and responds with allow, deny, or delay-then-allow.
func (s *Session) handleNotification(listenerFd int, req *seccomp.NotifReq, ruleMap map[int32][]*FaultRule) {
	syscallName := seccomp.SyscallName(req.Data.Nr)

	// For file syscalls, read the path argument for filtering and logging.
	var path string
	if IsFileSyscall(syscallName) {
		argIdx := 1 // openat and friends: arg1 is path
		if syscallName == "open" {
			argIdx = 0
		}
		p, err := seccomp.ReadStringFromProcess(req.PID, req.Data.Args[argIdx], 256)
		if err == nil {
			path = p
		}
	}

	decision := "allow"

	// Merge static + dynamic fault rules for this syscall.
	allRules := ruleMap[req.Data.Nr]
	if dynRules := s.getDynamicRules(req.Data.Nr); len(dynRules) > 0 {
		allRules = append(allRules, dynRules...)
	}

	// Check fault rules for this syscall.
	if len(allRules) > 0 {
		rules := allRules
		for _, rule := range rules {
			// Path-based filtering for file syscalls.
			if IsFileSyscall(syscallName) && path != "" {
				if rule.PathGlob != "" {
					if !rule.MatchPath(path) {
						dir := filepath.Dir(path)
						if !rule.MatchPath(dir + "/") {
							continue
						}
					}
				} else {
					if IsSystemPath(path) {
						decision = "allow (system path)"
						break
					}
				}
			}

			// Check stateful trigger (nth/after) — always increments counter.
			if !rule.ShouldFire() {
				continue
			}

			if rand.Float64() < rule.Probability {
				switch rule.Action {
				case ActionDelay:
					decision = fmt.Sprintf("delay(%s)", rule.Delay)
					s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
					time.Sleep(rule.Delay)
					if err := seccomp.Allow(listenerFd, req.ID); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to allow syscall after delay", slog.String("error", err.Error()))
						}
					}
					return

				case ActionDeny:
					decision = fmt.Sprintf("deny(%s)", rule.Errno)
					s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
					if err := seccomp.Deny(listenerFd, req.ID, int32(rule.Errno)); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to deny syscall", slog.String("error", err.Error()))
						}
					}
					return
				}
			}
		}
	}

	// Allow: let the kernel handle the syscall.
	s.logSyscall(slog.LevelDebug, syscallName, req.PID, decision, path)
	if err := seccomp.Allow(listenerFd, req.ID); err != nil {
		if !isClosedFdErr(err) {
			s.log.Error("failed to allow syscall", slog.String("error", err.Error()))
		}
	}
}

func (s *Session) logSyscall(level slog.Level, name string, pid uint32, decision, path string) {
	fields := []any{
		slog.String("name", name),
		slog.Int("pid", int(pid)),
		slog.String("decision", decision),
	}
	if path != "" {
		fields = append(fields, slog.String("path", path))
	}
	s.log.Log(context.Background(), level, "syscall intercepted", fields...)
}

func isClosedFdErr(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "bad file descriptor") ||
		strings.Contains(errStr, syscall.EBADF.Error()) ||
		strings.Contains(errStr, syscall.ENOENT.Error())
}
