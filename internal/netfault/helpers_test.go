package netfault

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// ─── packet construction ───────────────────────────────────────────────────

type pktOpts struct {
	srcIP, dstIP     string
	srcPort, dstPort uint16
	flags            uint8
	seq, ack         uint32
	window           uint16
	payload          []byte
	udp              bool
}

func defaultPkt() pktOpts {
	return pktOpts{
		srcIP: "10.0.0.2", dstIP: "10.99.0.5",
		srcPort: 40000, dstPort: 8080,
		flags: FlagACK, seq: 1000, ack: 2000, window: 65535,
	}
}

// makePacket builds a well-formed IPv4/TCP or IPv4/UDP packet with correct
// checksums, so tests exercise the same parse path production traffic does.
func makePacket(o pktOpts) *stack.PacketBuffer {
	src := tcpip.AddrFromSlice(parseIPv4(o.srcIP))
	dst := tcpip.AddrFromSlice(parseIPv4(o.dstIP))

	var transport []byte
	var proto tcpip.TransportProtocolNumber

	if o.udp {
		proto = header.UDPProtocolNumber
		transport = make([]byte, header.UDPMinimumSize+len(o.payload))
		u := header.UDP(transport)
		u.Encode(&header.UDPFields{
			SrcPort: o.srcPort,
			DstPort: o.dstPort,
			Length:  uint16(len(transport)),
		})
		copy(transport[header.UDPMinimumSize:], o.payload)
		u.SetChecksum(0)
		xsum := header.PseudoHeaderChecksum(proto, src, dst, uint16(len(transport)))
		u.SetChecksum(^checksum.Checksum(transport, xsum))
	} else {
		proto = header.TCPProtocolNumber
		transport = make([]byte, header.TCPMinimumSize+len(o.payload))
		t := header.TCP(transport)
		t.Encode(&header.TCPFields{
			SrcPort:    o.srcPort,
			DstPort:    o.dstPort,
			SeqNum:     o.seq,
			AckNum:     o.ack,
			DataOffset: header.TCPMinimumSize,
			Flags:      header.TCPFlags(o.flags),
			WindowSize: o.window,
		})
		copy(transport[header.TCPMinimumSize:], o.payload)
		t.SetChecksum(0)
		xsum := header.PseudoHeaderChecksum(proto, src, dst, uint16(len(transport)))
		t.SetChecksum(^checksum.Checksum(transport, xsum))
	}

	total := header.IPv4MinimumSize + len(transport)
	buf := make([]byte, total)
	ip := header.IPv4(buf)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total),
		TTL:         64,
		Protocol:    uint8(proto),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(buf[header.IPv4MinimumSize:], transport)

	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(buf),
	})
}

// ─── a recording lower endpoint ────────────────────────────────────────────

// recordEndpoint stands in for the wire. It records every packet the
// FaultEndpoint hands down and lets a test inject inbound traffic.
type recordEndpoint struct {
	mu       sync.Mutex
	written  []PacketView
	raw      [][]byte
	attached stack.NetworkDispatcher
	closed   bool
}

// makeRawBuffer wraps arbitrary bytes as a packet payload, for tests that need
// to push something deliberately malformed through the endpoint.
func makeRawBuffer(b []byte) buffer.Buffer { return buffer.MakeWithData(b) }

func (r *recordEndpoint) MTU() uint32                    { return 1500 }
func (r *recordEndpoint) MaxHeaderLength() uint16        { return 0 }
func (r *recordEndpoint) LinkAddress() tcpip.LinkAddress { return "" }
func (r *recordEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return 0
}
func (r *recordEndpoint) Attach(d stack.NetworkDispatcher) { r.attached = d }
func (r *recordEndpoint) IsAttached() bool                 { return r.attached != nil }
func (r *recordEndpoint) Wait()                            {}
func (r *recordEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}
func (r *recordEndpoint) AddHeader(*stack.PacketBuffer)        {}
func (r *recordEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }
func (r *recordEndpoint) SetOnCloseAction(func())              {}
func (r *recordEndpoint) SetLinkAddress(tcpip.LinkAddress)     {}
func (r *recordEndpoint) SetMTU(uint32)                        {}
func (r *recordEndpoint) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *recordEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range pkts.AsSlice() {
		if pv, ok := Parse(p, DirS2C); ok {
			r.written = append(r.written, pv)
		}
		if v := p.ToView(); v != nil {
			b := append([]byte(nil), v.AsSlice()...)
			r.raw = append(r.raw, b)
		}
		n++
	}
	return n, nil
}

func (r *recordEndpoint) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.written)
}

func (r *recordEndpoint) views() []PacketView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PacketView, len(r.written))
	copy(out, r.written)
	return out
}

// recordDispatcher records packets delivered up into the stack (ingress).
type recordDispatcher struct {
	mu sync.Mutex
	// delivered counts every delivery, parseable or not. count() must not be
	// derived from len(received): a deliberately malformed packet parses to
	// nothing, so counting parses would report a passed-through runt as
	// dropped and silently invert the assertion.
	delivered int
	received  []PacketView
}

func (d *recordDispatcher) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivered++
	if pv, ok := Parse(pkt, DirC2S); ok {
		d.received = append(d.received, pv)
	}
}

func (d *recordDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func (d *recordDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delivered
}

func (d *recordDispatcher) views() []PacketView {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]PacketView, len(d.received))
	copy(out, d.received)
	return out
}

// newTestEndpoint wires a FaultEndpoint between a recording dispatcher and a
// recording lower endpoint.
func newTestEndpoint(t *testing.T, opts Options) (*FaultEndpoint, *recordDispatcher, *recordEndpoint) {
	t.Helper()
	lower := &recordEndpoint{}
	fe := New(lower, opts)
	disp := &recordDispatcher{}
	fe.Attach(disp)
	t.Cleanup(fe.Close)
	return fe, disp, lower
}

// mustRules builds a RuleSet or fails the test.
func mustRules(t *testing.T, rules ...*Rule) *RuleSet {
	t.Helper()
	rs, err := NewRuleSet(rules...)
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	return rs
}

func dirPtr(d Direction) *Direction { return &d }

// ─── fake clock ────────────────────────────────────────────────────────────

// fakeClock drives deferQueue without sleeping. Advance() fires every timer
// whose deadline has passed, so a test asserts on ordering rather than racing
// wall-clock time.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	fn       func()
	// stopped is atomic because the deferQueue goroutine calls Stop()
	// concurrently with the test goroutine calling Advance(), which reads it
	// under the clock's mutex. Guarding only one side of that pair is not
	// guarding it: -race caught this intermittently, so CI would have too,
	// eventually and at the least convenient moment.
	stopped atomic.Bool
	seq     int
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{deadline: c.now.Add(d), fn: fn, seq: len(c.timers)}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	// Swap, not load-then-store: two concurrent Stop()s must not both report
	// that they were the one to stop the timer.
	return !t.stopped.Swap(true)
}

// pending reports how many live timers are registered.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped.Load() {
			n++
		}
	}
	return n
}

// queueLen reports how many items are waiting in the defer queue's heap.
func queueLen(q *deferQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.h)
}

// advanceWhenArmed advances a fake clock only once the defer queue has taken
// delivery of `want` items and armed its timer for them.
//
// Both halves of that wait are load-bearing, and each corresponds to a way the
// naive `clock.Advance(d)` silently does nothing:
//
//   - Items still being scheduled. Every deadline is computed as
//     clock.Now()+d at schedule time. Advancing mid-loop means the remaining
//     items are stamped from the ALREADY-ADVANCED now, so their deadlines land
//     a full interval past anything the test will ever advance to.
//
//   - Timer not yet armed. run() computes `wait` against the current time and
//     then calls AfterFunc. Advancing between those two steps registers the
//     timer at the advanced now — same outcome, deadline out of reach.
//
// Either way the packet is never released and the test fails claiming the
// datapath dropped it. That is a race in the harness, not in the code under
// test, and it is exactly the sort of failure that gets waved off as flaky and
// re-run until green. CI caught it on linux/amd64 where it reproduces; it does
// not reproduce on darwin/arm64 at all.
func advanceWhenArmed(t *testing.T, c *fakeClock, q *deferQueue, want int, d time.Duration) {
	t.Helper()
	if !waitFor(t, 2*time.Second, func() bool {
		return queueLen(q) >= want && c.pending() > 0
	}) {
		t.Fatalf("defer queue never settled: %d/%d items queued, %d timers armed; "+
			"Advance(%v) would have fired nothing", queueLen(q), want, c.pending(), d)
	}
	c.Advance(d)
}

// Advance moves the clock and fires due timers in (deadline, seq) order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*fakeTimer
	var keep []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped.Load() && !t.deadline.After(now) {
			due = append(due, t)
		} else if !t.stopped.Load() {
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()

	sort.SliceStable(due, func(i, j int) bool {
		if due[i].deadline.Equal(due[j].deadline) {
			return due[i].seq < due[j].seq
		}
		return due[i].deadline.Before(due[j].deadline)
	})
	for _, t := range due {
		t.fn()
	}
}

// waitFor polls cond until it holds or the deadline passes. Used only where a
// background goroutine (the deferQueue) must observe a state change; every
// assertion about *ordering* uses the fake clock instead.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}
