package protocol

import (
	"context"
	"fmt"

	"github.com/alicebob/miniredis/v2"
)

// ServeMock implements MockHandler for Redis. It stands up an in-process
// Redis server via github.com/alicebob/miniredis/v2 — a battle-tested
// Go implementation of the RESP protocol that real clients (go-redis,
// redigo) speak to without modification.
//
// Configuration keys (from spec.Config):
//
//	state: map[string]any — key → string value, pre-seeded into the server
//	  before it begins accepting connections. Values are coerced with
//	  fmt.Sprint to match Redis's "everything is a string" semantics.
//
// Routes are ignored — Redis mocks are state-driven, not route-driven.
// Users should fault specific commands via fault() + the real-service
// recipes, not through mock overrides.
func (p *redisProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	mr := miniredis.NewMiniRedis()
	if err := mr.StartAddr(addr); err != nil {
		return fmt.Errorf("mock redis start %s: %w", addr, err)
	}

	seeded := 0
	if raw, ok := spec.Config["state"]; ok {
		if state, ok := raw.(map[string]any); ok {
			for k, v := range state {
				_ = mr.Set(k, fmt.Sprint(v))
				seeded++
			}
		}
	}

	emitWith(emit, "started", map[string]string{
		"addr":   addr,
		"seeded": fmt.Sprintf("%d", seeded),
	})

	<-ctx.Done()
	mr.Close()
	emitWith(emit, "stopped", nil)
	return nil
}
