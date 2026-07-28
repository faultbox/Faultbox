package netfault

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Event is emitted for every packet a rule acted on. The runtime turns these
// into `packet` entries in the event log; the field names mirror the existing
// `proxy` event conventions, including the action/protocol fields added in
// v0.13.3.
type Event struct {
	Action    Action
	Direction Direction
	Protocol  Protocol
	Src       string
	Dst       string
	Len       int
	Flags     []string
	Flow      string
	RuleLabel string
}

// OnEvent receives packet-fault events. Must not block the datapath.
type OnEvent func(Event)

// FaultEndpoint wraps a stack.LinkEndpoint and applies packet fault rules to
// traffic in both directions.
//
// Modeled on gvisor.dev/gvisor/pkg/tcpip/link/sniffer, which implements the
// same decorator shape for observation. The difference is that this one is
// allowed to drop, defer, mutate, or answer.
//
// Rules live behind an atomic pointer so fault(), fault_stop() and the test
// runner can swap them without taking a lock on the datapath.
type FaultEndpoint struct {
	stack.LinkEndpoint

	dispatcher stack.NetworkDispatcher
	rules      atomic.Pointer[RuleSet]
	deferQ     *deferQueue
	reorder    *reorderBuffer
	onEvent    OnEvent
	decider    ProbabilityDecider

	// rndMu guards rnd: math/rand.Rand is not safe for concurrent use, and
	// the datapath is concurrent. A shared seeded source is required for
	// reproducibility, so the lock is the price.
	rndMu sync.Mutex
	rnd   *rand.Rand

	// whereEvals counts Starlark predicate evaluations so the runtime can warn
	// when a spec is paying far more for a lambda than the author expects.
	whereEvals atomic.Int64

	// mutationsSkipped counts corrupt/window rules that matched but could not
	// be applied because the packet spanned multiple buffer views. Surfaced
	// rather than swallowed: a fault that silently does nothing is the failure
	// mode this release is built to avoid.
	mutationsSkipped atomic.Int64

	flows   sync.Map // flow string -> *atomic.Int64 (per-flow packet ordinal)
	stopped atomic.Bool

	// clock is retained so the shaper can read the same time source the defer
	// queue schedules against; a pacer computing waits from wall time while
	// the queue runs on a fake clock would be untestable.
	clock Clock

	// Link shapers (RFC-054 v0.14.1). Nil when unset — the common case — so
	// an unshaped link pays one atomic load per packet.
	shapeC2S atomic.Pointer[shaper]
	shapeS2C atomic.Pointer[shaper]
	// mtuOverride is 0 when the lower endpoint's MTU stands.
	mtuOverride atomic.Uint32
}

// Options configures a FaultEndpoint.
type Options struct {
	// Seed drives probabilistic rules. Same seed + same rules + same traffic
	// produces the same decisions.
	Seed int64
	// OnEvent receives fault events; may be nil.
	OnEvent OnEvent
	// Decider is the RFC-042 §8.9 plan-leaf oracle; may be nil.
	Decider ProbabilityDecider
	// Clock is injectable for tests; nil means wall clock.
	Clock Clock
}

// New wraps lower in a FaultEndpoint.
func New(lower stack.LinkEndpoint, opts Options) *FaultEndpoint {
	clock := opts.Clock
	if clock == nil {
		clock = RealClock
	}
	return &FaultEndpoint{
		LinkEndpoint: lower,
		clock:        clock,
		deferQ:       newDeferQueue(opts.Clock),
		reorder:      newReorderBuffer(),
		onEvent:      opts.OnEvent,
		decider:      opts.Decider,
		rnd:          rand.New(rand.NewSource(opts.Seed)),
	}
}

// SetRules installs a rule set, replacing any previous one. Passing nil clears
// all rules, restoring pass-through.
func (e *FaultEndpoint) SetRules(rs *RuleSet) { e.rules.Store(rs) }

// Rules returns the installed rule set, or nil.
func (e *FaultEndpoint) Rules() *RuleSet { return e.rules.Load() }

// WhereEvaluations reports how many times a Starlark predicate ran.
func (e *FaultEndpoint) WhereEvaluations() int64 { return e.whereEvals.Load() }

// MutationsSkipped reports how many corrupt/window rules matched but could not
// be applied. Non-zero means a fault the spec asked for did not happen, and
// the runtime must say so rather than report a clean pass.
func (e *FaultEndpoint) MutationsSkipped() int64 { return e.mutationsSkipped.Load() }

// SetBandwidth paces one or both directions at bytesPerSec, holding at most
// maxBacklog worth of traffic before it starts dropping. A rate of 0 clears
// the shaper for that direction.
func (e *FaultEndpoint) SetBandwidth(dir Direction, bytesPerSec float64, maxBacklog time.Duration) {
	set := func(p *atomic.Pointer[shaper]) {
		if bytesPerSec <= 0 {
			p.Store(nil)
			return
		}
		p.Store(newShaper(bytesPerSec, maxBacklog))
	}
	switch dir {
	case DirC2S:
		set(&e.shapeC2S)
	case DirS2C:
		set(&e.shapeS2C)
	default:
		set(&e.shapeC2S)
		set(&e.shapeS2C)
	}
}

// SetMTU overrides the link MTU. Zero restores the lower endpoint's.
func (e *FaultEndpoint) SetMTU(mtu uint32) { e.mtuOverride.Store(mtu) }

// MTU reports the link MTU, honouring any override.
//
// This is what makes mtu() a real small-MTU path rather than a size filter:
// netstack derives the TCP MSS it advertises and its IP fragmentation
// threshold from this value, so lowering it makes the peer send smaller
// segments instead of making oversized ones vanish.
func (e *FaultEndpoint) MTU() uint32 {
	if v := e.mtuOverride.Load(); v > 0 {
		return v
	}
	return e.LinkEndpoint.MTU()
}

// ShaperStatsFor reports what a direction's shaper did, and whether one is
// installed at all.
func (e *FaultEndpoint) ShaperStatsFor(dir Direction) (ShaperStats, bool) {
	var sh *shaper
	if dir == DirC2S {
		sh = e.shapeC2S.Load()
	} else {
		sh = e.shapeS2C.Load()
	}
	if sh == nil {
		return ShaperStats{}, false
	}
	return sh.stats(), true
}

// ClearShapers removes both bandwidth shapers and any MTU override.
func (e *FaultEndpoint) ClearShapers() {
	e.shapeC2S.Store(nil)
	e.shapeS2C.Store(nil)
	e.mtuOverride.Store(0)
}

// paced wraps a release callback with this direction's bandwidth shaper.
//
// Applied before the rule pipeline runs, and therefore also when no rules are
// installed: bandwidth is a property of the link, so a spec that sets a rate
// and no rules must still see a slow link.
func (e *FaultEndpoint) paced(dir Direction, release func(*stack.PacketBuffer)) func(*stack.PacketBuffer) {
	var sh *shaper
	if dir == DirC2S {
		sh = e.shapeC2S.Load()
	} else {
		sh = e.shapeS2C.Load()
	}
	if sh == nil {
		return release
	}
	return func(p *stack.PacketBuffer) {
		wait, ok := sh.admit(p.Size(), e.clock.Now())
		if !ok {
			// Queue full. A real bottleneck drops here, and the drop is the
			// signal that makes the sender back off.
			return
		}
		if wait <= 0 {
			release(p)
			return
		}
		held := p.IncRef()
		if !e.deferQ.schedule(wait, func() {
			release(held)
			held.DecRef()
		}) {
			release(held)
			held.DecRef()
		}
	}
}

// Close drains deferred and held packets, then closes the wrapped endpoint.
// Deferred packets are flushed rather than discarded — silently dropping them
// would look like packet loss the spec never asked for.
func (e *FaultEndpoint) Close() {
	if e.stopped.Swap(true) {
		return
	}
	for _, release := range e.reorder.drain() {
		release()
	}
	e.deferQ.stop()
	e.LinkEndpoint.Close()
}

// Attach inserts this endpoint between the stack and the lower endpoint so
// inbound packets route through DeliverNetworkPacket below.
func (e *FaultEndpoint) Attach(d stack.NetworkDispatcher) {
	e.dispatcher = d
	e.LinkEndpoint.Attach(e)
}

// IsAttached reports whether a dispatcher is installed.
func (e *FaultEndpoint) IsAttached() bool { return e.dispatcher != nil }

// DeliverNetworkPacket handles ingress: SUT → Faultbox.
func (e *FaultEndpoint) DeliverNetworkPacket(proto tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	if e.dispatcher == nil {
		return
	}
	e.handle(pkt, DirC2S,
		// forward: up into the local stack.
		func(p *stack.PacketBuffer) { e.dispatcher.DeliverNetworkPacket(proto, p) },
		// reverse: back out the wire, toward the SUT that sent this.
		func(p *stack.PacketBuffer) { e.writeDown(p) },
	)
}

// writeDown sends a single synthesized packet to the lower endpoint.
func (e *FaultEndpoint) writeDown(p *stack.PacketBuffer) {
	var l stack.PacketBufferList
	l.PushBack(p.IncRef())
	defer l.DecRef()
	e.LinkEndpoint.WritePackets(l)
}

// DeliverLinkPacket forwards link-layer delivery untouched. Packet faults act
// at L3/L4; there is nothing to match here.
func (e *FaultEndpoint) DeliverLinkPacket(proto tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	if e.dispatcher != nil {
		e.dispatcher.DeliverLinkPacket(proto, pkt)
	}
}

// WritePackets handles egress: Faultbox → SUT.
//
// The lower endpoint's contract is to report how many packets it accepted, so
// a dropped packet must still be counted as written. Reporting a short count
// would make netstack treat it as backpressure and retry, which is not what
// "drop" means — the SUT should observe loss, not the sender.
func (e *FaultEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	batch := &egressBatch{e: e}
	defer batch.seal()

	total := pkts.Len()
	for _, pkt := range pkts.AsSlice() {
		e.handle(pkt, DirS2C,
			// forward: on down the wire toward the SUT.
			batch.release,
			// reverse: up into the local stack, so netstack sees the peer
			// (the SUT) as having sent it.
			func(p *stack.PacketBuffer) {
				if e.dispatcher != nil {
					e.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, p)
				}
			},
		)
	}
	return total, nil
}

// egressBatch routes a released packet either into the current write batch or,
// once that batch is gone, straight down the wire.
//
// The distinction is the difference between delaying a packet and losing it.
// Actions that hold a packet — delay, reorder, and bandwidth pacing — release
// it later, from the defer queue's timer goroutine, long after WritePackets
// has returned. The previous implementation gave `release` a closure appending
// to a local list that was written once the loop finished and DecRef'd on
// return, so every late release appended to a list nobody would ever write:
// `packet_delay(dir="s2c")` was a silent drop wearing a delay's name. Both
// delay tests drove the ingress path, where release goes straight to the
// dispatcher, so nothing caught it.
type egressBatch struct {
	e *FaultEndpoint

	mu     sync.Mutex
	list   stack.PacketBufferList
	sealed bool
}

// release is safe to call from any goroutine at any time, including after
// seal.
func (b *egressBatch) release(p *stack.PacketBuffer) {
	b.mu.Lock()
	if !b.sealed {
		b.list.PushBack(p.IncRef())
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	// The batch is closed; this packet was held past the write. Send it on its
	// own rather than dropping it.
	b.e.writeDown(p)
}

// seal writes whatever the batch accumulated and marks it closed, so any
// later release takes the direct path.
func (b *egressBatch) seal() {
	b.mu.Lock()
	b.sealed = true
	list := b.list
	b.list = stack.PacketBufferList{}
	b.mu.Unlock()

	defer list.DecRef()
	if list.Len() == 0 {
		return
	}
	b.e.LinkEndpoint.WritePackets(list)
}

// handle runs one packet through the rule pipeline.
//
// release sends the packet onward in its original direction. reverse sends a
// synthesized packet back toward whoever sent this one — a RST has to travel
// the opposite way, and routing it through release would inject it into the
// wrong stack entirely.
//
// Unparseable packets pass through untouched: a rule cannot meaningfully match
// what we could not parse.
func (e *FaultEndpoint) handle(pkt *stack.PacketBuffer, dir Direction, release, reverse func(*stack.PacketBuffer)) {
	// Shaping wraps release before anything else, so it applies even on the
	// no-rules fast path below. bandwidth() describes the link, not a packet.
	release = e.paced(dir, release)

	rs := e.rules.Load()
	if rs == nil || len(rs.rules) == 0 || e.stopped.Load() {
		release(pkt)
		return
	}
	pv, ok := Parse(pkt, dir)
	if !ok {
		release(pkt)
		return
	}
	pv.Index = e.nextIndex(pv.Flow)

	rule := rs.decide(&pv, e.randFloat, e.decider, func() { e.whereEvals.Add(1) })

	// A packet passing through advances any reorder hold on its flow.
	if fn := e.reorder.advance(pv.Flow); fn != nil {
		defer fn()
	}

	if rule == nil {
		release(pkt)
		return
	}

	e.emit(rule, &pv)

	switch rule.Action {
	case ActionPass:
		release(pkt)

	case ActionDrop:
		// Deliberately no RST. See ActionDrop's doc comment.

	case ActionDelay:
		held := pkt.IncRef()
		if !e.deferQ.schedule(rule.Delay, func() {
			release(held)
			held.DecRef()
		}) {
			release(held)
			held.DecRef()
		}

	case ActionDuplicate:
		for i := 0; i < rule.DuplicateCount; i++ {
			release(pkt)
		}

	case ActionReorder:
		held := pkt.IncRef()
		if !e.reorder.hold(pv.Flow, rule.ReorderBy, func() {
			release(held)
			held.DecRef()
		}) {
			release(held)
			held.DecRef()
		}

	case ActionCorrupt:
		e.corrupt(pkt, rule, &pv)
		release(pkt)

	case ActionReset:
		if rst := e.buildRST(&pv); rst != nil {
			reverse(rst)
			rst.DecRef()
		}
		// Original is dropped: the connection is being torn down.

	case ActionWindow:
		e.rewriteWindow(pkt, rule)
		release(pkt)

	default:
		release(pkt)
	}
}

func (e *FaultEndpoint) nextIndex(flow string) int {
	v, _ := e.flows.LoadOrStore(flow, &atomic.Int64{})
	return int(v.(*atomic.Int64).Add(1)) - 1
}

func (e *FaultEndpoint) randFloat() float64 {
	e.rndMu.Lock()
	defer e.rndMu.Unlock()
	return e.rnd.Float64()
}

func (e *FaultEndpoint) emit(rule *Rule, pv *PacketView) {
	if e.onEvent == nil {
		return
	}
	e.onEvent(Event{
		Action:    rule.Action,
		Direction: pv.Dir,
		Protocol:  pv.Proto,
		Src:       pv.SrcIP,
		Dst:       pv.DstIP,
		Len:       pv.PayloadLen,
		Flags:     FlagNames(pv.Flags),
		Flow:      pv.Flow,
		RuleLabel: rule.Label,
	})
}

// corrupt mutates payload bytes in place and then either repairs or
// deliberately breaks the transport checksum.
func (e *FaultEndpoint) corrupt(pkt *stack.PacketBuffer, rule *Rule, pv *PacketView) {
	b, aliased := PacketBytes(pkt)
	if !aliased {
		// The packet spans multiple views, so writes would land on a copy and
		// vanish. Count it instead of pretending the fault fired.
		e.mutationsSkipped.Add(1)
		return
	}
	if len(b) < header.IPv4MinimumSize {
		return
	}
	ip := header.IPv4(b)
	hlen := int(ip.HeaderLength())
	if hlen > len(b) {
		return
	}
	rest := b[hlen:]

	var payloadOff int
	switch pv.Proto {
	case ProtoTCP:
		if len(rest) < header.TCPMinimumSize {
			return
		}
		payloadOff = int(header.TCP(rest).DataOffset())
	case ProtoUDP:
		if len(rest) < header.UDPMinimumSize {
			return
		}
		payloadOff = header.UDPMinimumSize
	default:
		return
	}
	if payloadOff > len(rest) {
		return
	}
	payload := rest[payloadOff:]

	start := rule.CorruptOffset
	if start >= len(payload) {
		return // nothing to corrupt; the packet is shorter than the rule targets
	}
	end := start + rule.CorruptLength
	if end > len(payload) {
		end = len(payload)
	}

	switch rule.CorruptMode {
	case CorruptZero:
		for i := start; i < end; i++ {
			payload[i] = 0
		}
	case CorruptRandom:
		e.rndMu.Lock()
		for i := start; i < end; i++ {
			payload[i] = byte(e.rnd.Intn(256))
		}
		e.rndMu.Unlock()
	default: // CorruptFlip
		for i := start; i < end; i++ {
			payload[i] ^= 0xFF
		}
	}

	if rule.Checksum == ChecksumFix {
		recomputeChecksum(ip, rest, pv.Proto)
	}
	// ChecksumBreak: leave the stale checksum so the receiver discards it.
}

// recomputeChecksum repairs the transport checksum after a payload mutation,
// so the receiver accepts the packet with wrong data.
func recomputeChecksum(ip header.IPv4, rest []byte, proto Protocol) {
	src := ip.SourceAddress()
	dst := ip.DestinationAddress()
	switch proto {
	case ProtoTCP:
		t := header.TCP(rest)
		t.SetChecksum(0)
		xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, src, dst, uint16(len(rest)))
		t.SetChecksum(^checksum.Checksum(rest, xsum))
	case ProtoUDP:
		u := header.UDP(rest)
		u.SetChecksum(0)
		xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, src, dst, uint16(len(rest)))
		u.SetChecksum(^checksum.Checksum(rest, xsum))
	}
}

// rewriteWindow rewrites the advertised TCP receive window and repairs the
// checksum. Setting it to zero is the backpressure-stall scenario.
func (e *FaultEndpoint) rewriteWindow(pkt *stack.PacketBuffer, rule *Rule) {
	b, aliased := PacketBytes(pkt)
	if !aliased {
		e.mutationsSkipped.Add(1)
		return
	}
	if len(b) < header.IPv4MinimumSize {
		return
	}
	ip := header.IPv4(b)
	hlen := int(ip.HeaderLength())
	if hlen > len(b) {
		return
	}
	rest := b[hlen:]
	if ip.TransportProtocol() != header.TCPProtocolNumber || len(rest) < header.TCPMinimumSize {
		return
	}
	header.TCP(rest).SetWindowSize(rule.WindowSize)
	recomputeChecksum(ip, rest, ProtoTCP)
}

// buildRST synthesizes a TCP RST for the packet's flow, sent back toward the
// sender. Sequence numbers follow RFC 793: a RST in response to a segment
// carrying data acknowledges it, so the peer accepts the reset instead of
// discarding it as out of window.
func (e *FaultEndpoint) buildRST(pv *PacketView) *stack.PacketBuffer {
	if pv.Proto != ProtoTCP {
		return nil
	}
	src := tcpip.AddrFromSlice(parseIPv4(pv.DstIP))
	dst := tcpip.AddrFromSlice(parseIPv4(pv.SrcIP))
	if src.Len() == 0 || dst.Len() == 0 {
		return nil
	}

	const ipLen = header.IPv4MinimumSize
	const tcpLen = header.TCPMinimumSize
	buf := make([]byte, ipLen+tcpLen)

	ip := header.IPv4(buf[:ipLen])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen + tcpLen),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())

	ackNum := pv.Seq + uint32(pv.PayloadLen)
	if pv.HasFlags(FlagSYN) || pv.HasFlags(FlagFIN) {
		ackNum++
	}
	t := header.TCP(buf[ipLen:])
	t.Encode(&header.TCPFields{
		SrcPort:    pv.DstPort,
		DstPort:    pv.SrcPort,
		SeqNum:     pv.Ack,
		AckNum:     ackNum,
		DataOffset: tcpLen,
		Flags:      header.TCPFlagRst | header.TCPFlagAck,
		WindowSize: 0,
	})
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, src, dst, tcpLen)
	t.SetChecksum(^checksum.Checksum(buf[ipLen:], xsum))

	rst := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(buf),
	})
	// Link endpoints dispatch on this field — pipe.deliverPackets reads it to
	// pick the receiving stack's network protocol. Leaving it zero makes the
	// peer silently discard a perfectly well-formed RST.
	rst.NetworkProtocolNumber = header.IPv4ProtocolNumber
	return rst
}

func parseIPv4(s string) []byte {
	var out [4]byte
	n, field := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
			if n > 255 {
				return nil
			}
		case c == '.':
			if field >= 3 {
				return nil
			}
			out[field] = byte(n)
			field, n = field+1, 0
		default:
			return nil
		}
	}
	if field != 3 {
		return nil
	}
	out[3] = byte(n)
	return out[:]
}
