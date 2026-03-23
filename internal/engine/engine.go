// Package engine is the core Faultbox runtime. It manages sessions — isolated
// executions of target binaries under controlled conditions.
//
// The engine is designed as a library so that multiple frontends (CLI, HTTP API,
// interactive terminal, desktop UI) can use the same API.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/faultbox/Faultbox/internal/logging"
)

// Engine manages Faultbox sessions.
type Engine struct {
	log      *slog.Logger
	mu       sync.Mutex
	sessions map[string]*Session
}

// New creates a new Engine.
func New(logger *slog.Logger) *Engine {
	return &Engine{
		log:      logging.WithComponent(logger, "engine"),
		sessions: make(map[string]*Session),
	}
}

// Run creates a session and executes the target binary. It blocks until the
// target exits or the context is cancelled.
func (e *Engine) Run(ctx context.Context, cfg SessionConfig) (*Result, error) {
	session, err := newSession(cfg, e.log)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	e.mu.Lock()
	e.sessions[session.ID] = session
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		delete(e.sessions, session.ID)
		e.mu.Unlock()
	}()

	return session.run(ctx)
}

// Status returns the current state of a session.
func (e *Engine) Status(id string) (State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[id]
	if !ok {
		return "", false
	}
	return s.State(), true
}

// Sessions returns the IDs of all active sessions.
func (e *Engine) Sessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	return ids
}
