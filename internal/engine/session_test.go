package engine

import (
	"sync"
	"testing"
	"time"
)

func TestSyscallEventCallback(t *testing.T) {
	var mu sync.Mutex
	var captured []SyscallEvent

	cfg := SessionConfig{
		Binary: "/bin/true",
		OnSyscall: func(evt SyscallEvent) {
			mu.Lock()
			captured = append(captured, evt)
			mu.Unlock()
		},
	}

	s := &Session{
		ID:      "test-session",
		Service: "test-svc",
		cfg:     cfg,
	}

	// Emit some events.
	s.emitSyscallEvent("write", 123, "allow", "", 0)
	s.emitSyscallEvent("connect", 123, "deny(ECONNREFUSED)", "", 0)
	s.emitSyscallEvent("openat", 123, "delay(500ms)", "/data/test.db", 500*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(captured) != 3 {
		t.Fatalf("expected 3 events, got %d", len(captured))
	}

	// Check sequence numbering.
	if captured[0].Seq != 1 || captured[1].Seq != 2 || captured[2].Seq != 3 {
		t.Fatalf("unexpected seq: %d, %d, %d", captured[0].Seq, captured[1].Seq, captured[2].Seq)
	}

	// Check service attribution.
	if captured[0].Service != "test-svc" {
		t.Fatalf("expected service 'test-svc', got %q", captured[0].Service)
	}

	// Check fields.
	if captured[1].Decision != "deny(ECONNREFUSED)" {
		t.Fatalf("event[1].Decision = %q", captured[1].Decision)
	}
	if captured[2].Path != "/data/test.db" {
		t.Fatalf("event[2].Path = %q", captured[2].Path)
	}
	if captured[2].Latency != 500*time.Millisecond {
		t.Fatalf("event[2].Latency = %v", captured[2].Latency)
	}
}

func TestSyscallEventCallbackNil(t *testing.T) {
	// No callback set — should not panic.
	s := &Session{
		ID:  "test-session",
		cfg: SessionConfig{Binary: "/bin/true"},
	}
	s.emitSyscallEvent("write", 1, "allow", "", 0)
}
