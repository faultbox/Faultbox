package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
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
	ID    string
	cfg   SessionConfig
	log   *slog.Logger
	state State
	mu    sync.RWMutex
}

func newSession(cfg SessionConfig, parentLog *slog.Logger) (*Session, error) {
	id, err := generateID()
	if err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}

	return &Session{
		ID:    id,
		cfg:   cfg,
		log:   parentLog.With(slog.String("session_id", id)),
		state: StateCreated,
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

// run launches the target via the unified shim path.
// Platform-specific implementation in launch_linux.go / launch_other.go.
func (s *Session) run(ctx context.Context) (*Result, error) {
	return s.launch(ctx)
}

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
