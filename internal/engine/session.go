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

	// Monotonic syscall event counter.
	syscallSeq atomic.Int64

	// Dynamic fault rules — can be modified while session is running.
	dynamicRulesMu sync.RWMutex
	dynamicRules   map[int32][]*FaultRule
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

	return &Session{
		ID:    id,
		cfg:   cfg,
		log:   parentLog.With(slog.String("session_id", id)),
		state: StateCreated,
		rng:   rng,
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
func (s *Session) SetDynamicFaultRules(rules []FaultRule) {
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

// ClearDynamicFaultRules removes all dynamic fault rules.
func (s *Session) ClearDynamicFaultRules() {
	s.dynamicRulesMu.Lock()
	defer s.dynamicRulesMu.Unlock()
	s.dynamicRules = nil
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

// randFloat64 returns a deterministic random float using the session's seeded RNG.
// Thread-safe — called from notification handler goroutines.
func (s *Session) randFloat64() float64 {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.Float64()
}

// emitSyscallEvent sends a syscall event to the OnSyscall callback if set.
func (s *Session) emitSyscallEvent(syscallName string, pid uint32, decision, path string, latency time.Duration) {
	if s.cfg.OnSyscall == nil {
		return
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
	})
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
