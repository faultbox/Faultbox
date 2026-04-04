package engine

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// MatchPath checks if the given path matches the fault rule's path glob.
// Returns true if:
//   - the rule has no path glob (matches everything, but system paths are excluded separately)
//   - the path matches the glob pattern
func (r FaultRule) MatchPath(path string) bool {
	if r.PathGlob == "" {
		return true
	}
	matched, _ := filepath.Match(r.PathGlob, path)
	if matched {
		return true
	}
	// Also try matching just the prefix for directory globs like /data/*
	// filepath.Match requires exact segment match, so /data/* matches /data/foo
	// but not /data/foo/bar. For the PoC this is fine.
	return false
}

// FaultAction describes what kind of fault to inject.
type FaultAction int

const (
	// ActionDeny returns an errno to the caller (existing behavior).
	ActionDeny FaultAction = iota
	// ActionDelay sleeps for a duration then allows the syscall to proceed.
	ActionDelay
	// ActionHold blocks the syscall until explicitly released via a HoldQueue.
	ActionHold
	// ActionTrace allows the syscall but logs it at Info level (for observation without faulting).
	ActionTrace
)

// FaultTrigger controls when a stateful fault fires relative to a call count.
type FaultTrigger int

const (
	// TriggerAlways fires on every matching call (default).
	TriggerAlways FaultTrigger = iota
	// TriggerNth fires only on the Nth matching call (1-indexed).
	TriggerNth
	// TriggerAfter fires on all matching calls after the first N succeed.
	TriggerAfter
)

// FaultRule describes a fault to inject on a specific syscall.
type FaultRule struct {
	// Syscall name (e.g., "open", "openat", "write", "connect").
	Syscall string
	// Action to take when the fault fires.
	Action FaultAction
	// Errno to return when Action is ActionDeny.
	Errno syscall.Errno
	// Delay duration when Action is ActionDelay.
	Delay time.Duration
	// Probability of the fault firing, 0.0 to 1.0.
	Probability float64
	// PathGlob is an optional glob pattern for file syscalls (openat, etc.).
	// If set, only syscalls targeting paths matching the glob are faulted.
	// If empty, all paths are faulted (with system path exclusion).
	PathGlob string
	// Trigger controls when the fault fires based on call count.
	Trigger FaultTrigger
	// TriggerN is the count parameter for Nth/After triggers.
	TriggerN int
	// DestAddr filters connect() syscalls by destination address ("ip:port").
	// Only used for network partition modeling.
	DestAddr string
	// Label is an optional human-readable description (e.g., "WAL write").
	Label string
	// HoldTag identifies the HoldQueue this rule feeds into (ActionHold only).
	HoldTag string
	// counter tracks matching calls for stateful triggers (thread-safe).
	// Pointer so FaultRule remains safely copyable.
	counter *atomic.Int64
}

// ShouldFire checks stateful triggers and returns true if the fault should
// fire on this call. Must be called once per matching call (increments counter).
func (r *FaultRule) ShouldFire() bool {
	if r.counter == nil {
		r.counter = &atomic.Int64{}
	}
	n := r.counter.Add(1)
	switch r.Trigger {
	case TriggerNth:
		return n == int64(r.TriggerN)
	case TriggerAfter:
		return n > int64(r.TriggerN)
	default:
		return true
	}
}

// SystemPathPrefixes are paths excluded from file-related faults by default.
// These protect the dynamic linker, virtual filesystems, and device nodes.
var SystemPathPrefixes = []string{
	"/lib/",
	"/lib64/",
	"/usr/lib/",
	"/usr/lib64/",
	"/proc/",
	"/sys/",
	"/dev/",
	"/etc/ld.so.",
}

// ParseFaultRule parses a fault rule string.
//
// Deny format:  "syscall=ERRNO:PROBABILITY%[:PATH_GLOB][:TRIGGER]"
// Delay format: "syscall=delay:DURATION:PROBABILITY%[:TRIGGER]"
//
// Triggers: nth=N (fire on Nth call), after=N (fire after N calls succeed)
//
// Examples:
//
//	"open=ENOENT:50%"                  → fail open() with ENOENT 50% of the time
//	"write=EIO:100%"                   → fail every write() with EIO
//	"openat=ENOENT:100%:/data/*"       → fail opens under /data/ only
//	"connect=ECONNREFUSED:10%"         → reject 10% of connections
//	"connect=delay:200ms:100%"         → delay every connect() by 200ms
//	"fsync=EIO:100%:after=2"           → allow first 2 fsyncs, fail the rest
//	"openat=ENOENT:100%:/data/*:nth=3" → fail only the 3rd open under /data/
func ParseFaultRule(s string) (FaultRule, error) {
	// Split on '='
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: expected syscall=ACTION:PARAMS", s)
	}

	syscallName := strings.TrimSpace(parts[0])
	if syscallName == "" {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: empty syscall name", s)
	}

	// Split into segments: ACTION:PARAM1[:PARAM2...]
	// Allow up to 5 segments for deny+path+trigger or delay+trigger
	rest := strings.SplitN(parts[1], ":", 5)
	if len(rest) < 2 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: expected ACTION:PARAMS after '='", s)
	}

	actionStr := strings.TrimSpace(rest[0])

	// Check if this is a delay rule: "delay:DURATION:PROB%[:TRIGGER]"
	if strings.ToLower(actionStr) == "delay" {
		return parseDelayRule(s, syscallName, rest[1:])
	}

	// Otherwise it's a deny rule: "ERRNO:PROB%[:PATH_GLOB][:TRIGGER]"
	return parseDenyRule(s, syscallName, rest)
}

func parseDelayRule(raw, syscallName string, segments []string) (FaultRule, error) {
	// segments: [DURATION, PROB%, TRIGGER?]
	if len(segments) < 2 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: delay requires DURATION:PROB%%", raw)
	}

	durationStr := strings.TrimSpace(segments[0])
	delay, err := time.ParseDuration(durationStr)
	if err != nil {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: bad duration %q: %w", raw, durationStr, err)
	}
	if delay < 0 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: delay must be positive", raw)
	}

	probStr := strings.TrimSpace(segments[1])
	probStr = strings.TrimSuffix(probStr, "%")
	prob, err := strconv.ParseFloat(probStr, 64)
	if err != nil {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: bad probability: %w", raw, err)
	}
	prob /= 100.0

	if prob < 0 || prob > 1 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: probability must be 0-100%%", raw)
	}

	// Parse optional trigger.
	trigger, triggerN, err := parseTriggerFromSegments(raw, segments[2:])
	if err != nil {
		return FaultRule{}, err
	}

	return FaultRule{
		Syscall:     syscallName,
		Action:      ActionDelay,
		Delay:       delay,
		Probability: prob,
		Trigger:     trigger,
		TriggerN:    triggerN,
	}, nil
}

func parseDenyRule(raw, syscallName string, segments []string) (FaultRule, error) {
	// segments: [ERRNO, PROB%, PATH_GLOB?, TRIGGER?]
	errnoStr := strings.TrimSpace(segments[0])
	errno, ok := errnoByName(errnoStr)
	if !ok {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: unknown errno %q", raw, errnoStr)
	}

	probStr := strings.TrimSpace(segments[1])
	probStr = strings.TrimSuffix(probStr, "%")
	prob, err := strconv.ParseFloat(probStr, 64)
	if err != nil {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: bad probability: %w", raw, err)
	}
	prob /= 100.0

	if prob < 0 || prob > 1 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: probability must be 0-100%%", raw)
	}

	// Parse remaining segments: could be path glob, trigger, or both.
	var pathGlob string
	var triggerSegments []string

	for _, seg := range segments[2:] {
		seg = strings.TrimSpace(seg)
		if isTriggerSegment(seg) {
			triggerSegments = append(triggerSegments, seg)
		} else if seg != "" {
			pathGlob = seg
		}
	}

	trigger, triggerN, err := parseTriggerFromSegments(raw, triggerSegments)
	if err != nil {
		return FaultRule{}, err
	}

	return FaultRule{
		Syscall:     syscallName,
		Action:      ActionDeny,
		Errno:       errno,
		Probability: prob,
		PathGlob:    pathGlob,
		Trigger:     trigger,
		TriggerN:    triggerN,
	}, nil
}

// isTriggerSegment returns true if the segment looks like a trigger (nth=N or after=N).
func isTriggerSegment(s string) bool {
	return strings.HasPrefix(s, "nth=") || strings.HasPrefix(s, "after=")
}

// parseTriggerFromSegments extracts a trigger from remaining segments.
func parseTriggerFromSegments(raw string, segments []string) (FaultTrigger, int, error) {
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "nth=") {
			n, err := strconv.Atoi(strings.TrimPrefix(seg, "nth="))
			if err != nil || n < 1 {
				return TriggerAlways, 0, fmt.Errorf("invalid fault rule %q: bad nth value %q (must be >= 1)", raw, seg)
			}
			return TriggerNth, n, nil
		}
		if strings.HasPrefix(seg, "after=") {
			n, err := strconv.Atoi(strings.TrimPrefix(seg, "after="))
			if err != nil || n < 0 {
				return TriggerAlways, 0, fmt.Errorf("invalid fault rule %q: bad after value %q (must be >= 0)", raw, seg)
			}
			return TriggerAfter, n, nil
		}
	}
	return TriggerAlways, 0, nil
}

// ParseFaultRules parses multiple fault rule strings.
func ParseFaultRules(rules []string) ([]FaultRule, error) {
	result := make([]FaultRule, 0, len(rules))
	for _, r := range rules {
		rule, err := ParseFaultRule(r)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	return result, nil
}

func (r FaultRule) String() string {
	var s string
	switch r.Action {
	case ActionDelay:
		s = fmt.Sprintf("%s=delay:%s:%.0f%%", r.Syscall, r.Delay, r.Probability*100)
	default:
		s = fmt.Sprintf("%s=%s:%.0f%%", r.Syscall, errnoName(r.Errno), r.Probability*100)
		if r.PathGlob != "" {
			s += ":" + r.PathGlob
		}
	}
	switch r.Trigger {
	case TriggerNth:
		s += fmt.Sprintf(":nth=%d", r.TriggerN)
	case TriggerAfter:
		s += fmt.Sprintf(":after=%d", r.TriggerN)
	}
	return s
}

// IsFileSyscall returns true if the syscall inspects file paths (openat, etc.).
func IsFileSyscall(name string) bool {
	switch name {
	case "open", "openat", "creat", "mkdirat", "unlinkat", "faccessat",
		"fstatat", "readlinkat", "renameat", "renameat2", "linkat", "symlinkat":
		return true
	}
	return false
}

// IsFdSyscall returns true if the syscall operates on a file descriptor
// (first arg is fd) and could benefit from fd→path resolution.
func IsFdSyscall(name string) bool {
	switch name {
	case "write", "writev", "pwrite64", "pwritev", "pwritev2",
		"read", "readv", "pread64", "preadv", "preadv2",
		"fsync", "fdatasync", "ftruncate", "fchmod", "fchown":
		return true
	}
	return false
}

// IsSystemPath returns true if the path is a system path that should be
// excluded from file-related faults by default.
func IsSystemPath(path string) bool {
	for _, prefix := range SystemPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// errnoByName maps common errno names to values.
func errnoByName(name string) (syscall.Errno, bool) {
	name = strings.ToUpper(name)
	e, ok := errnoMap[name]
	return e, ok
}

func errnoName(e syscall.Errno) string {
	for name, val := range errnoMap {
		if val == e {
			return name
		}
	}
	return fmt.Sprintf("errno(%d)", int(e))
}

var errnoMap = map[string]syscall.Errno{
	// File/IO errors
	"ENOENT":  syscall.ENOENT,
	"EACCES":  syscall.EACCES,
	"EPERM":   syscall.EPERM,
	"EIO":     syscall.EIO,
	"ENOSPC":  syscall.ENOSPC,
	"EROFS":   syscall.EROFS,
	"EEXIST":  syscall.EEXIST,
	"ENOTEMPTY": syscall.ENOTEMPTY,
	"ENFILE":  syscall.ENFILE,
	"EMFILE":  syscall.EMFILE,
	"EFBIG":   syscall.EFBIG,

	// Network errors
	"ECONNREFUSED":  syscall.ECONNREFUSED,
	"ECONNRESET":    syscall.ECONNRESET,
	"ECONNABORTED":  syscall.ECONNABORTED,
	"ETIMEDOUT":     syscall.ETIMEDOUT,
	"ENETUNREACH":   syscall.ENETUNREACH,
	"EHOSTUNREACH":  syscall.EHOSTUNREACH,
	"EADDRINUSE":    syscall.EADDRINUSE,
	"EADDRNOTAVAIL": syscall.EADDRNOTAVAIL,

	// Generic
	"EINTR":  syscall.EINTR,
	"EAGAIN": syscall.EAGAIN,
	"ENOMEM": syscall.ENOMEM,
	"EBUSY":  syscall.EBUSY,
	"EINVAL": syscall.EINVAL,
}
