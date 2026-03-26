package engine

import (
	"sync"
	"testing"
	"time"
)

func TestVirtualClockAdvance(t *testing.T) {
	vc := &VirtualClock{enabled: true}

	vc.Advance(100 * time.Millisecond)
	if got := vc.Elapsed(); got != 100*time.Millisecond {
		t.Fatalf("expected 100ms, got %v", got)
	}

	vc.Advance(200 * time.Millisecond)
	if got := vc.Elapsed(); got != 300*time.Millisecond {
		t.Fatalf("expected 300ms, got %v", got)
	}
}

func TestVirtualClockTimespec(t *testing.T) {
	vc := &VirtualClock{enabled: true}

	vc.Advance(2*time.Second + 500*time.Millisecond)

	sec, nsec := vc.Timespec()
	if sec != 2 {
		t.Fatalf("expected sec=2, got %d", sec)
	}
	if nsec != 500_000_000 {
		t.Fatalf("expected nsec=500000000, got %d", nsec)
	}
}

func TestVirtualClockConcurrent(t *testing.T) {
	vc := &VirtualClock{enabled: true}
	var wg sync.WaitGroup

	// 100 goroutines each advance by 1ms
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vc.Advance(1 * time.Millisecond)
		}()
	}
	wg.Wait()

	if got := vc.Elapsed(); got != 100*time.Millisecond {
		t.Fatalf("expected 100ms, got %v", got)
	}
}

func TestVirtualClockZeroStart(t *testing.T) {
	vc := &VirtualClock{enabled: true}
	if got := vc.Elapsed(); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
	sec, nsec := vc.Timespec()
	if sec != 0 || nsec != 0 {
		t.Fatalf("expected 0s+0ns, got %ds+%dns", sec, nsec)
	}
}
