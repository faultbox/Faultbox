package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
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
	// Stdout receives the target's stdout (nil = discard).
	Stdout io.Writer
	// Stderr receives the target's stderr (nil = discard).
	Stderr io.Writer
	// Namespaces to create for isolation.
	Namespaces NamespaceConfig
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

func (s *Session) run(ctx context.Context) (*Result, error) {
	start := time.Now()

	s.log.Info("starting session",
		slog.String("binary", s.cfg.Binary),
		slog.Any("args", s.cfg.Args),
	)

	s.setState(StateStarting)

	cmd := exec.CommandContext(ctx, s.cfg.Binary, s.cfg.Args...)
	cmd.Stdout = s.cfg.Stdout
	cmd.Stderr = s.cfg.Stderr

	// Apply namespace isolation.
	if err := s.applyNamespaces(cmd); err != nil {
		s.setState(StateFailed)
		return &Result{
			SessionID: s.ID,
			ExitCode:  -1,
			Duration:  time.Since(start),
			Error:     err,
		}, err
	}

	// Start the target process.
	if err := cmd.Start(); err != nil {
		s.setState(StateFailed)
		return &Result{
			SessionID: s.ID,
			ExitCode:  -1,
			Duration:  time.Since(start),
			Error:     fmt.Errorf("start target: %w", err),
		}, fmt.Errorf("start target: %w", err)
	}

	s.setState(StateRunning)
	s.log.Info("target started", slog.Int("pid", cmd.Process.Pid))

	// Wait for target to exit.
	err := cmd.Wait()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			s.log.Warn("target exited with error",
				slog.Int("exit_code", exitCode),
				slog.Duration("duration", duration),
			)
		} else {
			s.setState(StateFailed)
			return &Result{
				SessionID: s.ID,
				ExitCode:  -1,
				Duration:  duration,
				Error:     fmt.Errorf("wait target: %w", err),
			}, fmt.Errorf("wait target: %w", err)
		}
	}

	s.setState(StateStopped)
	s.log.Info("session completed",
		slog.Int("exit_code", exitCode),
		slog.Duration("duration", duration),
	)

	return &Result{
		SessionID: s.ID,
		ExitCode:  exitCode,
		Duration:  duration,
	}, nil
}

// applyNamespaces is implemented per-platform:
//   - namespace_linux.go: real namespace isolation via clone flags
//   - namespace_other.go: no-op stub for non-Linux development

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
