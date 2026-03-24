package engine

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
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
// Deny format:  "syscall=ERRNO:PROBABILITY%[:PATH_GLOB]"
// Delay format: "syscall=delay:DURATION:PROBABILITY%"
//
// Examples:
//
//	"open=ENOENT:50%"              → fail open() with ENOENT 50% of the time
//	"write=EIO:100%"               → fail every write() with EIO
//	"openat=ENOENT:100%:/data/*"   → fail opens under /data/ only
//	"connect=ECONNREFUSED:10%"     → reject 10% of connections
//	"connect=delay:200ms:100%"     → delay every connect() by 200ms
//	"sendto=delay:50ms:20%"        → delay 20% of sends by 50ms
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
	rest := strings.SplitN(parts[1], ":", 4)
	if len(rest) < 2 {
		return FaultRule{}, fmt.Errorf("invalid fault rule %q: expected ACTION:PARAMS after '='", s)
	}

	actionStr := strings.TrimSpace(rest[0])

	// Check if this is a delay rule: "delay:DURATION:PROB%"
	if strings.ToLower(actionStr) == "delay" {
		return parseDelayRule(s, syscallName, rest[1:])
	}

	// Otherwise it's a deny rule: "ERRNO:PROB%[:PATH_GLOB]"
	return parseDenyRule(s, syscallName, rest)
}

func parseDelayRule(raw, syscallName string, segments []string) (FaultRule, error) {
	// segments: [DURATION, PROB%]
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

	return FaultRule{
		Syscall:     syscallName,
		Action:      ActionDelay,
		Delay:       delay,
		Probability: prob,
	}, nil
}

func parseDenyRule(raw, syscallName string, segments []string) (FaultRule, error) {
	// segments: [ERRNO, PROB%, PATH_GLOB?]
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

	var pathGlob string
	if len(segments) >= 3 {
		pathGlob = strings.TrimSpace(segments[2])
	}

	return FaultRule{
		Syscall:     syscallName,
		Action:      ActionDeny,
		Errno:       errno,
		Probability: prob,
		PathGlob:    pathGlob,
	}, nil
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
	switch r.Action {
	case ActionDelay:
		return fmt.Sprintf("%s=delay:%s:%.0f%%", r.Syscall, r.Delay, r.Probability*100)
	default:
		s := fmt.Sprintf("%s=%s:%.0f%%", r.Syscall, errnoName(r.Errno), r.Probability*100)
		if r.PathGlob != "" {
			s += ":" + r.PathGlob
		}
		return s
	}
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
