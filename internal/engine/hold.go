package engine

import (
	"context"
	"sync"
)

// HoldDecision tells the notification handler how to respond to a held syscall.
type HoldDecision struct {
	Allow bool  // true = allow, false = deny with Errno
	Errno int32 // used when Allow is false
}

// HeldNotif represents a syscall notification that is being held.
// The notification handler goroutine is blocked on ReleaseCh.
type HeldNotif struct {
	ReqID       uint64
	ListenerFd  int
	SyscallName string
	PID         uint32
	Path        string
	ReleaseCh   chan HoldDecision
}

// HoldQueue is a thread-safe queue of held syscall notifications.
// Syscalls are enqueued when intercepted and dequeued when released.
type HoldQueue struct {
	mu      sync.Mutex
	pending []*HeldNotif
	closed  bool

	// cond is signalled whenever a new item is enqueued or the queue is closed.
	cond *sync.Cond
}

// NewHoldQueue creates a new hold queue.
func NewHoldQueue() *HoldQueue {
	q := &HoldQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue adds a held notification to the queue and signals waiters.
func (q *HoldQueue) Enqueue(h *HeldNotif) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		// Queue already closed — release immediately.
		h.ReleaseCh <- HoldDecision{Allow: true}
		return
	}
	q.pending = append(q.pending, h)
	q.cond.Broadcast()
}

// Wait blocks until at least count notifications are held, or ctx is cancelled.
func (q *HoldQueue) Wait(count int, ctx context.Context) error {
	// Monitor context cancellation in a separate goroutine.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast() // wake up Wait
		case <-done:
		}
	}()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) < count && !q.closed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		q.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// ReleaseOne releases the oldest held notification with the given decision.
// Returns false if the queue is empty.
func (q *HoldQueue) ReleaseOne(d HoldDecision) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return false
	}
	h := q.pending[0]
	q.pending = q.pending[1:]
	h.ReleaseCh <- d
	return true
}

// ReleaseAll releases all held notifications with the given decision.
func (q *HoldQueue) ReleaseAll(d HoldDecision) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, h := range q.pending {
		h.ReleaseCh <- d
	}
	q.pending = nil
}

// ReleaseByIndex releases a specific pending notification by index.
// Returns false if the index is out of bounds.
func (q *HoldQueue) ReleaseByIndex(i int, d HoldDecision) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i < 0 || i >= len(q.pending) {
		return false
	}
	h := q.pending[i]
	q.pending = append(q.pending[:i], q.pending[i+1:]...)
	h.ReleaseCh <- d
	return true
}

// Pending returns a snapshot of currently held notifications.
func (q *HoldQueue) Pending() []*HeldNotif {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*HeldNotif, len(q.pending))
	copy(out, q.pending)
	return out
}

// Len returns the number of currently held notifications.
func (q *HoldQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Close releases all pending notifications with Allow and prevents future enqueues.
func (q *HoldQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	for _, h := range q.pending {
		h.ReleaseCh <- HoldDecision{Allow: true}
	}
	q.pending = nil
	q.cond.Broadcast()
}
