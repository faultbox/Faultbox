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

// TestDynamicRuleActivityReportsMatchCounts verifies that
// DynamicRuleActivity() surfaces per-rule counters, separating rules
// that matched traffic from those that did not. This is the backbone of
// the fault_zero_traffic event the Starlark runtime emits at fault-window
// close (v0.9.4 — customer feedback on silently-ineffective injections).
func TestDynamicRuleActivityReportsMatchCounts(t *testing.T) {
	s := &Session{
		ID:  "test-session",
		cfg: SessionConfig{Binary: "/bin/true"},
	}

	// Populate dynamicRules directly (bypassing SetDynamicFaultRules which
	// resolves syscall names via seccomp and returns -1 on non-Linux).
	fired := &FaultRule{Syscall: "connect", Action: ActionDeny, Label: "fired"}
	silent := &FaultRule{Syscall: "sendto", Action: ActionDeny, Label: "silent"}
	_ = fired.ShouldFire()
	_ = fired.ShouldFire()
	s.dynamicRules = map[int32][]*FaultRule{
		1: {fired},
		2: {silent},
	}

	reports := s.DynamicRuleActivity()
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2: %+v", len(reports), reports)
	}

	byLabel := map[string]DynamicRuleReport{}
	for _, r := range reports {
		byLabel[r.Label] = r
	}
	if byLabel["fired"].MatchCount != 2 {
		t.Errorf("fired.MatchCount = %d, want 2", byLabel["fired"].MatchCount)
	}
	if byLabel["silent"].MatchCount != 0 {
		t.Errorf("silent.MatchCount = %d, want 0", byLabel["silent"].MatchCount)
	}
	if byLabel["silent"].Action != "deny" {
		t.Errorf("silent.Action = %q, want deny", byLabel["silent"].Action)
	}
}
