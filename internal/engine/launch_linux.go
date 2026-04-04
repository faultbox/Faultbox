//go:build linux

package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
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
	// External listener fd: container shim already installed the seccomp filter.
	// Skip binary launch, go straight to notification loop.
	if s.cfg.ExternalListenerFd >= 0 {
		return s.launchExternal(ctx)
	}

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
			s.handleNotification(ctx, listenerFd, req, ruleMap)
		}(req)
	}
}

// handleNotification processes a single seccomp notification: reads path args,
// checks hold rules, then fault rules, and responds with allow, deny, delay, or hold.
func (s *Session) handleNotification(ctx context.Context, listenerFd int, req *seccomp.NotifReq, ruleMap map[int32][]*FaultRule) {
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

	// Virtual time: intercept time syscalls and return synthetic values.
	if s.vclock != nil && s.vclock.enabled {
		if s.handleTimeSyscall(listenerFd, req, syscallName) {
			return
		}
	}

	// Check hold rules FIRST — these take priority over fault rules.
	if holdRules := s.getHoldRules(req.Data.Nr); len(holdRules) > 0 {
		for _, rule := range holdRules {
			if IsFileSyscall(syscallName) && path != "" && rule.PathGlob != "" {
				if !rule.MatchPath(path) {
					dir := filepath.Dir(path)
					if !rule.MatchPath(dir + "/") {
						continue
					}
				}
			}

			q := s.GetHoldQueue(rule.HoldTag)
			if q == nil {
				continue
			}

			decision := fmt.Sprintf("hold(%s)", rule.HoldTag)
			s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
			s.emitSyscallEvent(syscallName, req.PID, decision, path, 0)

			releaseCh := make(chan HoldDecision, 1)
			q.Enqueue(&HeldNotif{
				ReqID:       req.ID,
				ListenerFd:  listenerFd,
				SyscallName: syscallName,
				PID:         req.PID,
				Path:        path,
				ReleaseCh:   releaseCh,
			})

			// Block until released or context cancelled.
			select {
			case d := <-releaseCh:
				releaseDecision := "allow (released)"
				if d.Allow {
					if err := seccomp.Allow(listenerFd, req.ID); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to allow held syscall", slog.String("error", err.Error()))
						}
					}
				} else {
					releaseDecision = fmt.Sprintf("deny(%d) (released)", d.Errno)
					if err := seccomp.Deny(listenerFd, req.ID, d.Errno); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to deny held syscall", slog.String("error", err.Error()))
						}
					}
				}
				s.emitSyscallEvent(syscallName, req.PID, releaseDecision, path, 0)
			case <-ctx.Done():
				// Fail-safe: allow on shutdown.
				seccomp.Allow(listenerFd, req.ID)
			}
			return
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

			// Destination address filtering for connect() syscalls.
			if rule.DestAddr != "" && syscallName == "connect" {
				ip, port, err := seccomp.ReadSockaddrFromProcess(req.PID, req.Data.Args[1])
				if err != nil || fmt.Sprintf("%s:%d", ip, port) != rule.DestAddr {
					continue
				}
			}

			// Check stateful trigger (nth/after) — always increments counter.
			if !rule.ShouldFire() {
				continue
			}

			if s.randFloat64() < rule.Probability {
				switch rule.Action {
				case ActionDelay:
					decision = fmt.Sprintf("delay(%s)", rule.Delay)
					s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
					if s.vclock != nil && s.vclock.enabled {
						s.vclock.Advance(rule.Delay)
					} else {
						time.Sleep(rule.Delay)
					}
					s.emitSyscallEvent(syscallName, req.PID, decision, path, rule.Delay)
					if err := seccomp.Allow(listenerFd, req.ID); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to allow syscall after delay", slog.String("error", err.Error()))
						}
					}
					return

				case ActionDeny:
					decision = fmt.Sprintf("deny(%s)", rule.Errno)
					s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
					s.emitSyscallEvent(syscallName, req.PID, decision, path, 0)
					if err := seccomp.Deny(listenerFd, req.ID, int32(rule.Errno)); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to deny syscall", slog.String("error", err.Error()))
						}
					}
					return

				case ActionTrace:
					decision = "trace"
					s.logSyscall(slog.LevelInfo, syscallName, req.PID, decision, path)
					s.emitSyscallEvent(syscallName, req.PID, decision, path, 0)
					if err := seccomp.Allow(listenerFd, req.ID); err != nil {
						if !isClosedFdErr(err) {
							s.log.Error("failed to allow traced syscall", slog.String("error", err.Error()))
						}
					}
					return
				}
			}
		}
	}

	// Allow: let the kernel handle the syscall.
	s.logSyscall(slog.LevelDebug, syscallName, req.PID, decision, path)
	s.emitSyscallEvent(syscallName, req.PID, decision, path, 0)
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

// handleTimeSyscall handles nanosleep, clock_nanosleep, and clock_gettime
// under virtual time. Returns true if the syscall was handled.
func (s *Session) handleTimeSyscall(listenerFd int, req *seccomp.NotifReq, syscallName string) bool {
	switch syscallName {
	case "nanosleep":
		// nanosleep(const struct timespec *req, struct timespec *rem)
		// Read requested sleep duration from arg0.
		dur := s.readTimespec(req.PID, req.Data.Args[0])
		s.vclock.Advance(dur)

		// Write zero remaining time if rem pointer is non-null.
		if req.Data.Args[1] != 0 {
			s.writeZeroTimespec(req.PID, req.Data.Args[1])
		}

		decision := fmt.Sprintf("virtual_sleep(%s)", dur)
		s.logSyscall(slog.LevelDebug, syscallName, req.PID, decision, "")
		s.emitSyscallEvent(syscallName, req.PID, decision, "", dur)
		seccomp.ReturnValue(listenerFd, req.ID, 0) // return success without sleeping
		return true

	case "clock_nanosleep":
		// clock_nanosleep(clockid, flags, const struct timespec *req, struct timespec *rem)
		// flags (arg1): 0=relative, TIMER_ABSTIME=1=absolute
		dur := s.readTimespec(req.PID, req.Data.Args[2])
		if req.Data.Args[1]&1 != 0 { // TIMER_ABSTIME
			// Absolute: sleep = requested - current virtual elapsed
			currentNs := s.vclock.Elapsed().Nanoseconds()
			requestedNs := dur.Nanoseconds()
			if requestedNs > currentNs {
				dur = time.Duration(requestedNs-currentNs) * time.Nanosecond
			} else {
				dur = 0
			}
		}
		s.vclock.Advance(dur)

		if req.Data.Args[3] != 0 {
			s.writeZeroTimespec(req.PID, req.Data.Args[3])
		}

		decision := fmt.Sprintf("virtual_sleep(%s)", dur)
		s.logSyscall(slog.LevelDebug, syscallName, req.PID, decision, "")
		s.emitSyscallEvent(syscallName, req.PID, decision, "", dur)
		seccomp.ReturnValue(listenerFd, req.ID, 0)
		return true

	case "clock_gettime":
		// clock_gettime(clockid, struct timespec *tp)
		// Write virtual time to the output timespec.
		sec, nsec := s.vclock.Timespec()
		buf := make([]byte, 16)
		binary.LittleEndian.PutUint64(buf[0:8], uint64(sec))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(nsec))
		if err := seccomp.WriteToProcess(req.PID, req.Data.Args[1], buf); err != nil {
			s.log.Error("failed to write virtual time", slog.String("error", err.Error()))
			seccomp.Allow(listenerFd, req.ID) // fallback: let kernel handle it
			return true
		}

		decision := fmt.Sprintf("virtual_time(%ds+%dns)", sec, nsec)
		s.logSyscall(slog.LevelDebug, syscallName, req.PID, decision, "")
		s.emitSyscallEvent(syscallName, req.PID, decision, "", 0)
		seccomp.ReturnValue(listenerFd, req.ID, 0)
		return true
	}
	return false
}

// readTimespec reads a struct timespec {int64 tv_sec, int64 tv_nsec} from process memory.
func (s *Session) readTimespec(pid uint32, addr uint64) time.Duration {
	if addr == 0 {
		return 0
	}
	buf, err := seccomp.ReadFromProcess(pid, addr, 16)
	if err != nil || len(buf) < 16 {
		return 0
	}
	sec := int64(binary.LittleEndian.Uint64(buf[0:8]))
	nsec := int64(binary.LittleEndian.Uint64(buf[8:16]))
	return time.Duration(sec)*time.Second + time.Duration(nsec)*time.Nanosecond
}

// writeZeroTimespec writes a zero timespec to process memory.
func (s *Session) writeZeroTimespec(pid uint32, addr uint64) {
	buf := make([]byte, 16) // all zeros
	seccomp.WriteToProcess(pid, addr, buf)
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
