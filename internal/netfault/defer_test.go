package netfault

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// TestDeferQueuePreservesInsertionOrderOnTies is the load-bearing determinism
// property. Packets scheduled for the same instant must be released in arrival
// order. The naive implementation — one goroutine per delayed packet — cannot
// guarantee this, because goroutine wakeup order is unspecified.
func TestDeferQueuePreservesInsertionOrderOnTies(t *testing.T) {
	clock := newFakeClock()
	q := newDeferQueue(clock)
	defer q.stop()

	var mu sync.Mutex
	var order []int
	const n = 50
	for i := 0; i < n; i++ {
		i := i
		q.schedule(100*time.Millisecond, func() {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}

	clock.Advance(100 * time.Millisecond)
	if !waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == n
	}) {
		mu.Lock()
		got := len(order)
		mu.Unlock()
		t.Fatalf("released %d of %d packets", got, n)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, v := range order {
		if v != i {
			t.Fatalf("release order diverged at %d: got %v", i, order[:min(len(order), 10)])
		}
	}
}

func TestDeferQueueReleasesByDeadline(t *testing.T) {
	clock := newFakeClock()
	q := newDeferQueue(clock)
	defer q.stop()

	var mu sync.Mutex
	var order []string
	sched := func(name string, d time.Duration) {
		q.schedule(d, func() {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		})
	}

	// Scheduled out of deadline order on purpose.
	sched("late", 300*time.Millisecond)
	sched("early", 100*time.Millisecond)
	sched("mid", 200*time.Millisecond)

	clock.Advance(150 * time.Millisecond)
	if !waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 1 }) {
		t.Fatalf("after 150ms: %v", order)
	}
	clock.Advance(100 * time.Millisecond)
	if !waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 }) {
		t.Fatalf("after 250ms: %v", order)
	}
	clock.Advance(100 * time.Millisecond)
	if !waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 3 }) {
		t.Fatalf("after 350ms: %v", order)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"early", "mid", "late"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("release order = %v, want %v", order, want)
		}
	}
}

func TestDeferQueueDoesNotReleaseEarly(t *testing.T) {
	clock := newFakeClock()
	q := newDeferQueue(clock)
	defer q.stop()

	var fired bool
	var mu sync.Mutex
	q.schedule(time.Second, func() { mu.Lock(); fired = true; mu.Unlock() })

	clock.Advance(999 * time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Error("packet released before its deadline")
	}
}

// TestDeferQueueStopFlushes: teardown must not swallow in-flight packets.
// Dropping them would look like packet loss the spec never asked for.
func TestDeferQueueStopFlushes(t *testing.T) {
	clock := newFakeClock()
	q := newDeferQueue(clock)

	var mu sync.Mutex
	released := 0
	for i := 0; i < 5; i++ {
		q.schedule(time.Hour, func() { mu.Lock(); released++; mu.Unlock() })
	}
	if got := q.pending(); got != 5 {
		t.Fatalf("pending = %d, want 5", got)
	}
	q.stop()

	mu.Lock()
	defer mu.Unlock()
	if released != 5 {
		t.Errorf("stop() released %d of 5 pending packets", released)
	}
}

func TestDeferQueueScheduleAfterStop(t *testing.T) {
	q := newDeferQueue(newFakeClock())
	q.stop()
	if q.schedule(time.Millisecond, func() {}) {
		t.Error("schedule() succeeded on a stopped queue; caller cannot know to release the packet")
	}
}

// ─── endpoint-level delay & reorder ────────────────────────────────────────

func TestDelayReleasesAfterDuration(t *testing.T) {
	clock := newFakeClock()
	fe, disp, _ := newTestEndpoint(t, Options{Clock: clock})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDelay, Delay: 250 * time.Millisecond}))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))

	if got := disp.count(); got != 0 {
		t.Fatalf("packet delivered immediately (%d); delay did not hold it", got)
	}
	clock.Advance(250 * time.Millisecond)
	if !waitFor(t, 2*time.Second, func() bool { return disp.count() == 1 }) {
		t.Fatalf("packet not delivered after the delay elapsed (count=%d)", disp.count())
	}
}

func TestDelayPreservesOrder(t *testing.T) {
	clock := newFakeClock()
	fe, disp, _ := newTestEndpoint(t, Options{Clock: clock})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDelay, Delay: 100 * time.Millisecond}))

	const n = 20
	for i := 0; i < n; i++ {
		o := defaultPkt()
		o.seq = uint32(1000 + i)
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
	}
	clock.Advance(100 * time.Millisecond)
	if !waitFor(t, 2*time.Second, func() bool { return disp.count() == n }) {
		t.Fatalf("delivered %d of %d", disp.count(), n)
	}

	for i, v := range disp.views() {
		if want := uint32(1000 + i); v.Seq != want {
			t.Fatalf("delayed packet %d has seq %d, want %d — equal deadlines must release in arrival order", i, v.Seq, want)
		}
	}
}

func TestReorderByN(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	// Hold the first packet until 2 later ones have gone ahead of it.
	fe.SetRules(mustRules(t,
		&Rule{Action: ActionReorder, ReorderBy: 2, Trigger: TriggerNth, TriggerN: 1},
	))

	for i := 0; i < 4; i++ {
		o := defaultPkt()
		o.seq = uint32(1000 + i)
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
	}

	views := disp.views()
	if len(views) != 4 {
		t.Fatalf("delivered %d, want 4", len(views))
	}
	// Packet 0 is held; 1 and 2 pass; then 0 is released; then 3.
	want := []uint32{1001, 1002, 1000, 1003}
	for i, w := range want {
		if views[i].Seq != w {
			got := make([]uint32, len(views))
			for j, v := range views {
				got[j] = v.Seq
			}
			t.Fatalf("reorder sequence = %v, want %v", got, want)
		}
	}
}

func TestReorderDrainsOnClose(t *testing.T) {
	lower := &recordEndpoint{}
	fe := New(lower, Options{})
	disp := &recordDispatcher{}
	fe.Attach(disp)
	// Hold packet 1 behind 100 others that will never arrive.
	fe.SetRules(mustRules(t,
		&Rule{Action: ActionReorder, ReorderBy: 100, Trigger: TriggerNth, TriggerN: 1},
	))
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))

	if got := disp.count(); got != 0 {
		t.Fatalf("packet delivered early (%d)", got)
	}
	fe.Close()
	if got := disp.count(); got != 1 {
		t.Errorf("held packet was stranded on close: delivered %d, want 1", got)
	}
}

// ─── probability ───────────────────────────────────────────────────────────

// TestProbabilitySeedReproducible: the same seed must yield the same fire
// vector, or a .fb bundle cannot replay a probabilistic packet fault.
func TestProbabilitySeedReproducible(t *testing.T) {
	run := func(seed int64) []uint32 {
		fe, disp, _ := newTestEndpoint(t, Options{Seed: seed})
		fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Probability: 0.5}))
		for i := 0; i < 40; i++ {
			o := defaultPkt()
			o.seq = uint32(i)
			fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
		}
		out := []uint32{}
		for _, v := range disp.views() {
			out = append(out, v.Seq)
		}
		return out
	}
	a, b := run(42), run(42)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("same seed produced different survivors:\n  %v\n  %v", a, b)
	}
	if c := run(7); fmt.Sprint(a) == fmt.Sprint(c) {
		t.Error("different seeds produced identical survivors; the RNG is not being consulted")
	}
}

// TestDeciderPinsOutcome covers the RFC-042 §8.9 plan-leaf oracle, which must
// override the RNG for non-stochastic rules.
func TestDeciderPinsOutcome(t *testing.T) {
	var seen []int
	fe, disp, _ := newTestEndpoint(t, Options{
		Seed: 1,
		Decider: func(_ *Rule, occ int) (bool, bool) {
			seen = append(seen, occ)
			return occ%2 == 0, true // pin: fire on even occurrences
		},
	})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Probability: 0.5}))

	for i := 0; i < 6; i++ {
		o := defaultPkt()
		o.seq = uint32(i)
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
	}

	// Occurrences 0,2,4 fire (dropped); 1,3,5 survive.
	got := []uint32{}
	for _, v := range disp.views() {
		got = append(got, v.Seq)
	}
	want := []uint32{1, 3, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("survivors = %v, want %v", got, want)
	}
	wantOcc := []int{0, 1, 2, 3, 4, 5}
	if fmt.Sprint(seen) != fmt.Sprint(wantOcc) {
		t.Errorf("occurrence indices = %v, want %v (consumed exactly once each)", seen, wantOcc)
	}
}

// TestStochasticModeIgnoresDecider mirrors the syscall path: mode="stochastic"
// keeps the legacy RNG behaviour even when a decider is attached.
func TestStochasticModeIgnoresDecider(t *testing.T) {
	called := false
	fe, _, _ := newTestEndpoint(t, Options{
		Seed:    1,
		Decider: func(*Rule, int) (bool, bool) { called = true; return true, true },
	})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Probability: 0.5, Mode: "stochastic"}))

	for i := 0; i < 5; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))
	}
	if called {
		t.Error("decider was consulted for a stochastic rule")
	}
}

// TestZeroProbabilityFiresAlways pins the deliberate choice that an unset
// Probability means "always", so a struct-literal rule cannot silently no-op.
func TestZeroProbabilityFiresAlways(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop})) // Probability left unset

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))
	if got := disp.count(); got != 0 {
		t.Errorf("rule with unset probability did not fire (delivered %d) — silent no-op regression", got)
	}
}

// ─── where= accounting ─────────────────────────────────────────────────────

// TestWhereLambdaOnlyAfterDeclarative is the performance contract: a lambda
// must not be paid for on packets the cheap predicates already excluded.
func TestWhereLambdaOnlyAfterDeclarative(t *testing.T) {
	var calls int
	fe, _, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Match: Match{
			Port:  8080,
			Where: func(*PacketView) bool { calls++; return true },
		},
	}))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt())) // port 8080 → lambda runs
	other := defaultPkt()
	other.dstPort = 9999
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(other)) // excluded before the lambda

	if calls != 1 {
		t.Errorf("lambda evaluated %d times, want 1 (declarative predicates must gate it)", calls)
	}
	if got := fe.WhereEvaluations(); got != 1 {
		t.Errorf("WhereEvaluations = %d, want 1", got)
	}
}

func TestWhereLambdaReceivesPopulatedPacket(t *testing.T) {
	var got *PacketView
	fe, _, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Match: Match{Where: func(p *PacketView) bool {
			cp := *p
			got = &cp
			return true
		}},
	}))

	o := defaultPkt()
	o.payload = []byte("hello")
	o.flags = FlagPSH | FlagACK
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	if got == nil {
		t.Fatal("lambda never ran")
	}
	if got.Proto != ProtoTCP || got.DstPort != 8080 || got.PayloadLen != 5 {
		t.Errorf("packet passed to lambda is wrong: %s", got.String())
	}
	if !got.HasFlags(FlagPSH | FlagACK) {
		t.Errorf("flags not populated: %v", FlagNames(got.Flags))
	}
	if got.Dir != DirC2S {
		t.Errorf("Dir = %s, want c2s", got.Dir)
	}
}

// ─── concurrency ───────────────────────────────────────────────────────────

// TestConcurrentFlowsIsolated exercises the datapath under -race with many
// goroutines, which is how the shared RNG and flow table get stressed.
func TestConcurrentFlowsIsolated(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{Seed: 3})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Match: Match{Port: 9999}}))

	const goroutines, each = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				o := defaultPkt()
				o.srcPort = uint16(40000 + g)
				fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
			}
		}(g)
	}
	wg.Wait()

	if got, want := disp.count(), goroutines*each; got != want {
		t.Errorf("delivered %d, want %d (no packet should have matched port 9999)", got, want)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// PROBE: does delay work on EGRESS (S2C), or is it a silent drop?
//
// Both existing delay tests drive DeliverNetworkPacket (ingress). WritePackets
// builds a local `forward` list, hands `release` a closure that appends to it,
// writes the list once the loop finishes, and DecRefs it on return — so a
// release that fires later, from the defer queue's timer, appends to a list
// nobody will ever write.
func TestDelayOnEgressActuallyDelivers(t *testing.T) {
	clock := newFakeClock()
	fe, _, lower := newTestEndpoint(t, Options{Clock: clock})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDelay,
		Delay:  250 * time.Millisecond,
		Match:  Match{Dir: dirPtr(DirS2C)},
	}))

	writeOne(fe, makePacket(defaultPkt()))

	if got := lower.count(); got != 0 {
		t.Fatalf("packet written immediately (%d); the delay did not hold it", got)
	}
	clock.Advance(250 * time.Millisecond)
	if !waitFor(t, 2*time.Second, func() bool { return lower.count() == 1 }) {
		t.Fatalf("egress packet never written after the delay elapsed (count=%d) "+
			"— delay on dir=\"s2c\" is silently dropping instead of delaying", lower.count())
	}
}
