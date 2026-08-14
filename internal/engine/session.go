package engine

import (
	"context"
	"crypto/rand"
	"errors"
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
	Label    string        `json:"label,omitempty"`      // optional fault label from deny/delay
	Op       string        `json:"op,omitempty"`         // named operation (e.g., "persist")

	// DestIP / DestPort are populated only for connect() syscalls — read
	// from the SUT's sockaddr argument once at the top of handleNotification
	// and forwarded so the determinism layer (RFC-040 §8.1) can classify
	// network destinations as mediated / unmediated / DNS without reading
	// process memory a second time.
	DestIP   string `json:"dest_ip,omitempty"`
	DestPort int    `json:"dest_port,omitempty"`
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
	// ProbabilityDecider, when non-nil, is consulted for every probability
	// check before falling back to the seeded RNG (RFC-042 §8.9). Returns
	// (fire, pinned). When pinned=true the boolean determines firing; when
	// pinned=false the engine uses the RNG, preserving rc1 stochastic
	// behavior. Receives the rule pointer and the zero-based occurrence
	// index produced by FaultRule.NextProbabilityOccurrence.
	//
	// The decider is set by the spec layer based on the current PlanLeaf;
	// nil for tests that don't drive plan-tree fan-out, in which case the
	// firing path is identical to pre-rc2.
	ProbabilityDecider func(rule *FaultRule, occurrence int) (fire bool, pinned bool)
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
	// SupervisorError is set when the seccomp notification loop stopped
	// while the target was still running. The target keeps its filter, so
	// every intercepted syscall from that moment blocks forever — the test
	// is not merely slow, its result is meaningless. Callers must surface
	// this rather than reporting a timeout.
	SupervisorError error
	// DroppedNotifications counts notifications the kernel discarded before
	// the supervisor received them. Harmless individually; a large count
	// alongside odd behaviour is worth seeing.
	DroppedNotifications int64
}

// SupervisorFailure reports that the notification loop stopped early, and
// carries what the session knows about why.
func (s *Session) SupervisorFailure() error {
	if !s.supervisorFailed.Load() {
		return nil
	}
	msg := "seccomp notification loop stopped while the target was still running; " +
		"every intercepted syscall after that point blocks indefinitely"
	if p := s.supervisorErr.Load(); p != nil && *p != "" {
		msg += ": " + *p
	}
	return errors.New(msg)
}

// noteSupervisorExit records an early notification-loop exit. Called only
// when the target is known to still be alive.
func (s *Session) noteSupervisorExit(cause string) {
	s.supervisorFailed.Store(true)
	if cause != "" {
		s.supervisorErr.Store(&cause)
	}
}

// DroppedNotifications returns the count of notifications discarded before
// the supervisor could receive them.
func (s *Session) DroppedNotifications() int64 {
	return s.droppedNotifs.Load()
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

	// unresolvedPaths counts path-carrying syscalls whose path could not be
	// recovered from /proc. Surfaced in DynamicRuleReport so a zero-match
	// path-filtered rule can say *why* it never matched.
	unresolvedPaths atomic.Int64

	// droppedNotifs counts notifications the kernel had already discarded by
	// the time we called RECV (ENOENT — the target thread died or was
	// interrupted first). Each one is harmless on its own: the syscall the
	// notification described is gone, so there is nothing left to answer.
	// The count is reported at teardown so a run can say how often it
	// happened rather than leaving it to be inferred.
	droppedNotifs atomic.Int64

	// supervisorFailed records that the notification loop stopped while the
	// child was still running. A filtered process with no supervisor blocks
	// on every intercepted syscall forever, so this is never survivable and
	// must never be silent — Result.SupervisorError carries it to the
	// runtime, which fails the test with it.
	supervisorFailed atomic.Bool
	supervisorErr    atomic.Pointer[string]

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
		// Pre-initialize the atomic counters so the notification loop
		// never has to race lazily on first use. The lazy init in
		// ShouldFire / NextProbabilityOccurrence is benign when the
		// rule is only ever consulted by one goroutine, but the
		// notification handler can be invoked from multiple
		// goroutines for the same rule (issue B2 from the principal
		// review on PR #121).
		if rules[i].counter == nil {
			rules[i].counter = &atomic.Int64{}
		}
		if rules[i].probCounter == nil {
			rules[i].probCounter = &atomic.Int64{}
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

	// PathGlob is the rule's path filter, empty when it has none.
	PathGlob string
	// UnresolvedPaths is how many syscalls of this rule's family were seen
	// while the path could not be recovered from /proc.
	//
	// Without it, a zero-match path-filtered rule is ambiguous: the app might
	// have performed no matching I/O at all, or Faultbox might have failed to
	// read the path (the read races the SUT and truncates at 256 bytes) and so
	// never had anything to match against. Those need very different fixes, and
	// until now they looked identical.
	UnresolvedPaths int64
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
				Syscall:         r.Syscall,
				Action:          actionName(r.Action),
				Op:              r.Op,
				Label:           r.Label,
				MatchCount:      r.MatchCount(),
				PathGlob:        r.PathGlob,
				UnresolvedPaths: s.unresolvedPaths.Load(),
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
	s.emitSyscallEventDest(syscallName, pid, decision, path, latency, "", 0, extra...)
}

// emitSyscallEventDest is the connect-aware emit path used when
// handleNotification has already read the sockaddr destination. Non-connect
// callers go through emitSyscallEvent which passes "" / 0 for the dest pair.
// RFC-040 §8.1 — destination is captured here so the determinism layer can
// classify connect() events without re-reading process memory.
func (s *Session) emitSyscallEventDest(syscallName string, pid uint32, decision, path string, latency time.Duration, destIP string, destPort int, extra ...string) {
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
		DestIP:   destIP,
		DestPort: destPort,
	})
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
