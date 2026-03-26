package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHoldQueueEnqueueAndRelease(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	// Enqueue a held notification.
	h := &HeldNotif{
		ReqID:       1,
		SyscallName: "write",
		ReleaseCh:   make(chan HoldDecision, 1),
	}
	q.Enqueue(h)

	if q.Len() != 1 {
		t.Fatalf("expected 1 pending, got %d", q.Len())
	}

	// Release it.
	ok := q.ReleaseOne(HoldDecision{Allow: true})
	if !ok {
		t.Fatal("ReleaseOne returned false on non-empty queue")
	}

	d := <-h.ReleaseCh
	if !d.Allow {
		t.Fatal("expected Allow=true")
	}

	if q.Len() != 0 {
		t.Fatalf("expected 0 pending after release, got %d", q.Len())
	}
}

func TestHoldQueueReleaseAll(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	held := make([]*HeldNotif, 3)
	for i := range held {
		held[i] = &HeldNotif{
			ReqID:       uint64(i),
			SyscallName: "write",
			ReleaseCh:   make(chan HoldDecision, 1),
		}
		q.Enqueue(held[i])
	}

	if q.Len() != 3 {
		t.Fatalf("expected 3 pending, got %d", q.Len())
	}

	q.ReleaseAll(HoldDecision{Allow: false, Errno: 5})

	for i, h := range held {
		d := <-h.ReleaseCh
		if d.Allow {
			t.Fatalf("held[%d]: expected Allow=false", i)
		}
		if d.Errno != 5 {
			t.Fatalf("held[%d]: expected Errno=5, got %d", i, d.Errno)
		}
	}

	if q.Len() != 0 {
		t.Fatalf("expected 0 pending after ReleaseAll, got %d", q.Len())
	}
}

func TestHoldQueueWait(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Wait for 2 items in a goroutine.
	var wg sync.WaitGroup
	var waitErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitErr = q.Wait(2, ctx)
	}()

	// Enqueue first — Wait should not return yet.
	q.Enqueue(&HeldNotif{ReqID: 1, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)})
	time.Sleep(50 * time.Millisecond)
	if q.Len() != 1 {
		t.Fatalf("expected 1 pending, got %d", q.Len())
	}

	// Enqueue second — Wait should return.
	q.Enqueue(&HeldNotif{ReqID: 2, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)})
	wg.Wait()

	if waitErr != nil {
		t.Fatalf("Wait returned error: %v", waitErr)
	}
}

func TestHoldQueueWaitContextCancel(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var waitErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitErr = q.Wait(5, ctx)
	}()

	// Cancel context — Wait should return with error.
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	if waitErr == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestHoldQueueClose(t *testing.T) {
	q := NewHoldQueue()

	// Enqueue then close — should auto-release.
	h := &HeldNotif{ReqID: 1, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)}
	q.Enqueue(h)
	q.Close()

	d := <-h.ReleaseCh
	if !d.Allow {
		t.Fatal("Close should release with Allow=true")
	}

	// Enqueue after close — should release immediately.
	h2 := &HeldNotif{ReqID: 2, SyscallName: "read", ReleaseCh: make(chan HoldDecision, 1)}
	q.Enqueue(h2)

	d2 := <-h2.ReleaseCh
	if !d2.Allow {
		t.Fatal("Enqueue after Close should release with Allow=true")
	}
}

func TestHoldQueueReleaseOneEmpty(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	ok := q.ReleaseOne(HoldDecision{Allow: true})
	if ok {
		t.Fatal("ReleaseOne on empty queue should return false")
	}
}

func TestHoldQueueFIFOOrder(t *testing.T) {
	q := NewHoldQueue()
	defer q.Close()

	h1 := &HeldNotif{ReqID: 10, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)}
	h2 := &HeldNotif{ReqID: 20, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)}
	h3 := &HeldNotif{ReqID: 30, SyscallName: "write", ReleaseCh: make(chan HoldDecision, 1)}

	q.Enqueue(h1)
	q.Enqueue(h2)
	q.Enqueue(h3)

	// Release one at a time — should be FIFO.
	q.ReleaseOne(HoldDecision{Allow: true})
	d := <-h1.ReleaseCh
	if !d.Allow {
		t.Fatal("h1 should be released first")
	}

	q.ReleaseOne(HoldDecision{Allow: true})
	d = <-h2.ReleaseCh
	if !d.Allow {
		t.Fatal("h2 should be released second")
	}

	q.ReleaseOne(HoldDecision{Allow: true})
	d = <-h3.ReleaseCh
	if !d.Allow {
		t.Fatal("h3 should be released third")
	}
}
