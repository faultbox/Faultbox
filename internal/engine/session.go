package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faultbox/Faultbox/internal/seccomp"
)

// State represents the lifecycle of a session.
type State string

const (
	StateCreated  State = "created"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

// SyscallEvent captures a single intercepted syscall and the decision made.
type SyscallEvent struct {
	Seq      int64         `json:"seq"`
	Time     time.Time     `json:"time"`
	Service  string        `json:"service"`
	Syscall  string        `json:"syscall"`
	PID      uint32        `json:"pid"`
	Decision string        `json:"decision"` // "allow", "deny(ERRNO)", "delay(500ms)"
	Path     string        `json:"path,omitempty"`
	Latency  time.Duration `json:"latency_ns,omitempty"` // time spent in fault (delay duration)
	Label    string        `json:"label,omitempty"`       // optional fault label from deny/delay
	Op       string        `json:"op,omitempty"`          // named operation (e.g., "persist")
}

// VirtualClock tracks virtual time for a session. When enabled, fault delays
// advance the virtual clock instead of sleeping, and nanosleep/clock_nanosleep
// return immediately with the virtual clock advanced.
type VirtualClock struct {
	mu      sync.Mutex
	enabled bool
	elapsed time.Duration // total virtual time elapsed since session start
}

// Advance moves the virtual clock forward by d.
func (vc *VirtualClock) Advance(d time.Duration) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.elapsed += d
}

// Elapsed returns the current virtual time elapsed.
func (vc *VirtualClock) Elapsed() time.Duration {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.elapsed
}

// Timespec returns virtual time as seconds + nanoseconds (for clock_gettime injection).
func (vc *VirtualClock) Timespec() (sec int64, nsec int64) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	total := vc.elapsed.Nanoseconds()
	return total / 1e9, total % 1e9
}

// SessionConfig describes what to run and how to isolate it.
type SessionConfig struct {
	// Binary is the path to the target executable.
	Binary string
	// Args are the arguments to pass to the target.
	Args []string
	// Env is extra environment variables for the target (KEY=VALUE).
	// These are appended to the current process's environment.
	Env []string
	// Stdout receives the target's stdout (nil = discard).
	Stdout io.Writer
	// Stderr receives the target's stderr (nil = discard).
	Stderr io.Writer
	// Namespaces to create for isolation.
	Namespaces NamespaceConfig
	// FaultRules to apply via seccomp-notify interception.
	FaultRules []FaultRule
	// OnSyscall is called for every intercepted syscall (optional).
	// Must be safe to call from multiple goroutines.
	OnSyscall func(SyscallEvent)
	// Seed for deterministic probabilistic fault decisions.
	// If non-nil, used to seed the session's RNG. If nil, uses a random seed.
	Seed *uint64
	// VirtualTime enables virtual time for this session.
	// Fault delays advance the virtual clock instead of sleeping.
	VirtualTime bool
	// ExternalListenerFd is set when the seccomp listener was created externally
	// (e.g., by a container shim). When >= 0, the session skips binary launch and
	// runs only the notification loop on this fd.
	// IMPORTANT: Set to -1 for normal binary launch. Go's zero value (0) is a
	// valid fd and would incorrectly trigger the external path.
	ExternalListenerFd int
	// ExternalPID is the target process PID (host namespace) when using an
	// external listener. Used for process memory reads in the notification loop.
	ExternalPID int
}

// NamespaceConfig controls which Linux namespaces are created.
type NamespaceConfig struct {
	PID     bool // CLONE_NEWPID — isolated process tree
	Network bool // CLONE_NEWNET — isolated network stack
	Mount   bool // CLONE_NEWNS  — isolated mount table
	User    bool // CLONE_NEWUSER — unprivileged namespace creation
}

// DefaultNamespaces returns a config with all namespaces enabled.
func DefaultNamespaces() NamespaceConfig {
	return NamespaceConfig{
		PID:     true,
		Network: true,
		Mount:   true,
		User:    true,
	}
}

// Result captures the outcome of a session.
type Result struct {
	// SessionID is the unique session identifier.
	SessionID string
	// ExitCode is the target's exit code (-1 if killed by signal).
	ExitCode int
	// Duration is how long the target ran.
	Duration time.Duration
	// Error is set if the session failed to start or was killed.
	Error error
}

// Session is a single isolated execution of a target binary.
type Session struct {
	ID      string
	Service string // service name label (for event attribution)
	cfg     SessionConfig
	log     *slog.Logger
	state   State
	mu      sync.RWMutex

	// Deterministic RNG for probabilistic fault decisions.
	rng   *mathrand.Rand
	rngMu sync.Mutex

	// Virtual clock for time virtualization (nil if disabled).
	vclock *VirtualClock

	// Monotonic syscall event counter.
	syscallSeq atomic.Int64

	// Dynamic fault rules — can be modified while session is running.
	dynamicRulesMu sync.RWMutex
	dynamicRules   map[int32][]*FaultRule

	// Hold rules — separate from dynamic rules, managed by barrier/parallel.
	holdRulesMu sync.RWMutex
	holdRules   map[int32][]*FaultRule
	holdQueues  map[string]*HoldQueue
}

func NewSession(cfg SessionConfig, parentLog *slog.Logger) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	// Create deterministic RNG from seed.
	var rng *mathrand.Rand
	if cfg.Seed != nil {
		rng = mathrand.New(mathrand.NewPCG(*cfg.Seed, 0))
	} else {
		// Random seed for non-deterministic mode.
		rng = mathrand.New(mathrand.NewPCG(mathrand.Uint64(), mathrand.Uint64()))
	}

	var vclock *VirtualClock
	if cfg.VirtualTime {
		vclock = &VirtualClock{enabled: true}
	}

	return &Session{
		ID:     id,
		cfg:    cfg,
		log:    parentLog.With(slog.String("session_id", id)),
		state:  StateCreated,
		rng:    rng,
		vclock: vclock,
	}, nil
}

// State returns the current session state.
func (s *Session) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Session) setState(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.log.Info("state changed", slog.String("state", string(state)))
}

// Run launches the target via the unified shim path.
// Platform-specific implementation in launch_linux.go / launch_other.go.
func (s *Session) Run(ctx context.Context) (*Result, error) {
	return s.launch(ctx)
}

// SetDynamicFaultRules replaces the dynamic fault rules for the session.
// These rules are checked by the notification loop alongside static rules.
//
// Nil-safe on purpose: mock services and container services started
// with seccomp=False register no underlying *Session. Callers used to
// race ahead and dereference a nil receiver here (Freight #75.2 in
// v0.11.1 — a fault_matrix row targeting a mock panicked the whole
// suite). Guard at the method level so any future caller that forgets
// the mock check degrades gracefully rather than crashing.
func (s *Session) SetDynamicFaultRules(rules []FaultRule) {
	if s == nil {
		return
	}
	s.dynamicRulesMu.Lock()
	defer s.dynamicRulesMu.Unlock()
	ruleMap := make(map[int32][]*FaultRule)
	for i := range rules {
		nr := seccomp.SyscallNumber(rules[i].Syscall)
		if nr < 0 {
			continue
		}
		ruleMap[nr] = append(ruleMap[nr], &rules[i])
	}
	s.dynamicRules = ruleMap
}

// ClearDynamicFaultRules removes all dynamic fault rules. Nil-safe for
// the same reason as SetDynamicFaultRules above.
func (s *Session) ClearDynamicFaultRules() {
	if s == nil {
		return
	}
	s.dynamicRulesMu.Lock()
	defer s.dynamicRulesMu.Unlock()
	s.dynamicRules = nil
}

// DynamicRuleReport summarises one dynamic rule's activity over its window.
// Emitted when the fault is removed so callers can see which rules never
// matched any traffic — often a sign that the fault window didn't cover
// actual app I/O (e.g., client cached an init-time response and reused it).
// RFC-024 adjacent; shipped in v0.9.4.
type DynamicRuleReport struct {
	Syscall    string
	Action     string
	Op         string
	Label      string
	MatchCount int64
}

// DynamicRuleActivity returns one report per currently-installed dynamic
// fault rule, including its match counter. Safe to call before
// ClearDynamicFaultRules so callers can diff the counter snapshot.
func (s *Session) DynamicRuleActivity() []DynamicRuleReport {
	if s == nil {
		return nil
	}
	s.dynamicRulesMu.RLock()
	defer s.dynamicRulesMu.RUnlock()
	if s.dynamicRules == nil {
		return nil
	}
	var out []DynamicRuleReport
	for _, rules := range s.dynamicRules {
		for _, r := range rules {
			out = append(out, DynamicRuleReport{
				Syscall:    r.Syscall,
				Action:     actionName(r.Action),
				Op:         r.Op,
				Label:      r.Label,
				MatchCount: r.MatchCount(),
			})
		}
	}
	return out
}

// getDynamicRules returns dynamic rules for a syscall number.
func (s *Session) getDynamicRules(nr int32) []*FaultRule {
	s.dynamicRulesMu.RLock()
	defer s.dynamicRulesMu.RUnlock()
	if s.dynamicRules == nil {
		return nil
	}
	return s.dynamicRules[nr]
}

// RegisterHoldQueue creates a hold queue and returns it.
// The tag is used to link hold rules to the queue. Nil-safe.
func (s *Session) RegisterHoldQueue(tag string) *HoldQueue {
	if s == nil {
		return nil
	}
	s.holdRulesMu.Lock()
	defer s.holdRulesMu.Unlock()
	if s.holdQueues == nil {
		s.holdQueues = make(map[string]*HoldQueue)
	}
	q := NewHoldQueue()
	s.holdQueues[tag] = q
	return q
}

// GetHoldQueue returns the hold queue for the given tag. Nil-safe.
func (s *Session) GetHoldQueue(tag string) *HoldQueue {
	if s == nil {
		return nil
	}
	s.holdRulesMu.RLock()
	defer s.holdRulesMu.RUnlock()
	if s.holdQueues == nil {
		return nil
	}
	return s.holdQueues[tag]
}

// AddHoldRules adds hold rules for a tag. These are checked before fault rules.
// Nil-safe — mock services register a runningSession with session=nil.
func (s *Session) AddHoldRules(tag string, rules []FaultRule) {
	if s == nil {
		return
	}
	s.holdRulesMu.Lock()
	defer s.holdRulesMu.Unlock()
	if s.holdRules == nil {
		s.holdRules = make(map[int32][]*FaultRule)
	}
	for i := range rules {
		rules[i].HoldTag = tag
		nr := seccomp.SyscallNumber(rules[i].Syscall)
		if nr < 0 {
			continue
		}
		s.holdRules[nr] = append(s.holdRules[nr], &rules[i])
	}
}

// RemoveHoldRules removes all hold rules and closes the queue for a tag.
// Nil-safe — mock services and seccomp=False containers have no session.
func (s *Session) RemoveHoldRules(tag string) {
	if s == nil {
		return
	}
	s.holdRulesMu.Lock()
	defer s.holdRulesMu.Unlock()
	// Remove rules with this tag.
	for nr, rules := range s.holdRules {
		filtered := rules[:0]
		for _, r := range rules {
			if r.HoldTag != tag {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) == 0 {
			delete(s.holdRules, nr)
		} else {
			s.holdRules[nr] = filtered
		}
	}
	// Close and remove the queue.
	if q, ok := s.holdQueues[tag]; ok {
		q.Close()
		delete(s.holdQueues, tag)
	}
}

// CloseAllHoldQueues closes all hold queues (cleanup on session stop).
// Nil-safe so teardown paths don't panic on mock targets.
func (s *Session) CloseAllHoldQueues() {
	if s == nil {
		return
	}
	s.holdRulesMu.Lock()
	defer s.holdRulesMu.Unlock()
	for _, q := range s.holdQueues {
		q.Close()
	}
	s.holdQueues = nil
	s.holdRules = nil
}

// getHoldRules returns hold rules for a syscall number.
func (s *Session) getHoldRules(nr int32) []*FaultRule {
	s.holdRulesMu.RLock()
	defer s.holdRulesMu.RUnlock()
	if s.holdRules == nil {
		return nil
	}
	return s.holdRules[nr]
}

// randFloat64 returns a deterministic random float using the session's seeded RNG.
// Thread-safe — called from notification handler goroutines.
func (s *Session) randFloat64() float64 {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.Float64()
}

// emitSyscallEvent sends a syscall event to the OnSyscall callback if set.
// Optional extra args: labels[0]=label, labels[1]=op.
func (s *Session) emitSyscallEvent(syscallName string, pid uint32, decision, path string, latency time.Duration, extra ...string) {
	if s.cfg.OnSyscall == nil {
		return
	}
	var label, op string
	if len(extra) > 0 {
		label = extra[0]
	}
	if len(extra) > 1 {
		op = extra[1]
	}
	s.cfg.OnSyscall(SyscallEvent{
		Seq:      s.syscallSeq.Add(1),
		Time:     time.Now(),
		Service:  s.Service,
		Syscall:  syscallName,
		PID:      pid,
		Decision: decision,
		Path:     path,
		Latency:  latency,
		Label:    label,
		Op:       op,
	})
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
