package netfault

import (
	"container/heap"
	"sync"
	"time"
)

// Clock abstracts time so delay and reorder are testable without sleeping and
// so a future virtual-time mode can drive the queue directly. The production
// implementation is realClock; tests use a fake.
type Clock interface {
	Now() time.Time
	// AfterFunc schedules fn and returns a handle that can stop it.
	AfterFunc(d time.Duration, fn func()) Timer
}

// Timer is the subset of time.Timer the queue needs.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, fn func()) Timer {
	return time.AfterFunc(d, fn)
}

// RealClock is the default wall-clock implementation.
var RealClock Clock = realClock{}

// deferred is one packet waiting on a deadline.
type deferred struct {
	deadline time.Time
	// seq is a monotonically increasing insertion number. It breaks deadline
	// ties deterministically: two packets scheduled for the same instant are
	// always released in the order they arrived.
	//
	// This is the whole reason the queue exists as a single structure instead
	// of one goroutine per delayed packet. Goroutine wakeup order is not
	// specified, so the naive implementation would make packet reordering
	// nondeterministic — precisely the ambiguity L1 already refuses to promise
	// away for syscalls. Packet faults should be better than that, not equally
	// vague.
	seq   uint64
	fn    func()
	index int
}

type deferHeap []*deferred

func (h deferHeap) Len() int { return len(h) }
func (h deferHeap) Less(i, j int) bool {
	if h[i].deadline.Equal(h[j].deadline) {
		return h[i].seq < h[j].seq
	}
	return h[i].deadline.Before(h[j].deadline)
}
func (h deferHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *deferHeap) Push(x any) {
	d := x.(*deferred)
	d.index = len(*h)
	*h = append(*h, d)
}
func (h *deferHeap) Pop() any {
	old := *h
	n := len(old)
	d := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return d
}

// deferQueue releases packets at their deadlines from a single goroutine, in a
// deterministic order for a given seed.
type deferQueue struct {
	mu      sync.Mutex
	h       deferHeap
	seq     uint64
	clock   Clock
	timer   Timer
	wake    chan struct{}
	stopped bool
	wg      sync.WaitGroup
	done    chan struct{}
}

func newDeferQueue(clock Clock) *deferQueue {
	if clock == nil {
		clock = RealClock
	}
	q := &deferQueue{
		clock: clock,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	heap.Init(&q.h)
	q.wg.Add(1)
	go q.run()
	return q
}

// schedule enqueues fn to run after d. Returns false if the queue is stopped,
// in which case the caller must decide what to do with the packet (the
// endpoint releases it immediately rather than leaking it).
func (q *deferQueue) schedule(d time.Duration, fn func()) bool {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	q.seq++
	heap.Push(&q.h, &deferred{
		deadline: q.clock.Now().Add(d),
		seq:      q.seq,
		fn:       fn,
	})
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// run is the single goroutine that owns release ordering.
func (q *deferQueue) run() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if q.stopped {
			q.mu.Unlock()
			return
		}
		var wait time.Duration
		hasWork := false
		if len(q.h) > 0 {
			wait = q.h[0].deadline.Sub(q.clock.Now())
			hasWork = true
		}
		// Fire everything already due, in (deadline, seq) order.
		if hasWork && wait <= 0 {
			d := heap.Pop(&q.h).(*deferred)
			q.mu.Unlock()
			d.fn()
			continue
		}
		if q.timer != nil {
			q.timer.Stop()
			q.timer = nil
		}
		if hasWork {
			q.timer = q.clock.AfterFunc(wait, func() {
				select {
				case q.wake <- struct{}{}:
				default:
				}
			})
		}
		q.mu.Unlock()

		select {
		case <-q.wake:
		case <-q.done:
			return
		}
	}
}

// stop halts the queue and drains anything still pending by running it
// immediately. Dropping queued packets on teardown would look like packet loss
// the spec never asked for, so a stopping queue flushes rather than discards.
func (q *deferQueue) stop() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	q.stopped = true
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	pending := make([]*deferred, 0, len(q.h))
	for len(q.h) > 0 {
		pending = append(pending, heap.Pop(&q.h).(*deferred))
	}
	q.mu.Unlock()

	close(q.done)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	q.wg.Wait()

	for _, d := range pending {
		d.fn()
	}
}

// pending reports how many packets are waiting. Test-facing.
func (q *deferQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.h)
}

// reorderBuffer implements ActionReorder: hold a packet until `by` later
// packets on the same flow have been released ahead of it.
//
// Kept separate from deferQueue because reordering is triggered by packet
// arrivals, not by the clock — a held packet whose flow goes quiet must not
// sit forever, so the endpoint also flushes on teardown.
type reorderBuffer struct {
	mu   sync.Mutex
	held map[string]*heldPacket
}

type heldPacket struct {
	remaining int
	release   func()
}

func newReorderBuffer() *reorderBuffer {
	return &reorderBuffer{held: make(map[string]*heldPacket)}
}

// hold registers a packet to be released after `by` more packets on `flow`.
// Only one packet per flow is held at a time; if one is already held, the new
// packet passes through rather than stacking, which keeps the semantics
// explainable ("this segment arrives late") instead of turning into an
// unbounded shuffle.
func (rb *reorderBuffer) hold(flow string, by int, release func()) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if _, exists := rb.held[flow]; exists {
		return false
	}
	rb.held[flow] = &heldPacket{remaining: by, release: release}
	return true
}

// advance records that a packet passed on `flow`, releasing the held packet
// once enough have gone by. Returns the release func to invoke, or nil.
func (rb *reorderBuffer) advance(flow string) func() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	h, ok := rb.held[flow]
	if !ok {
		return nil
	}
	h.remaining--
	if h.remaining > 0 {
		return nil
	}
	delete(rb.held, flow)
	return h.release
}

// drain releases every held packet. Called on teardown so a quiet flow does
// not strand a segment.
func (rb *reorderBuffer) drain() []func() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	out := make([]func(), 0, len(rb.held))
	for k, h := range rb.held {
		out = append(out, h.release)
		delete(rb.held, k)
	}
	return out
}
