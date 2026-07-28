package netfault

import (
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// writeOne pushes a single packet through the egress path.
func writeOne(fe *FaultEndpoint, pkt *stack.PacketBuffer) {
	var l stack.PacketBufferList
	l.PushBack(pkt)
	defer l.DecRef()
	fe.WritePackets(l)
}

func TestFaultEndpointPassthrough(t *testing.T) {
	fe, disp, lower := newTestEndpoint(t, Options{})

	// No rules installed at all.
	payload := []byte("hello world")
	o := defaultPkt()
	o.payload = payload
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
	writeOne(fe, makePacket(o))

	if got := disp.count(); got != 1 {
		t.Errorf("ingress delivered = %d, want 1", got)
	}
	if got := lower.count(); got != 1 {
		t.Errorf("egress written = %d, want 1", got)
	}
	if got := disp.views()[0]; !bytes.Equal(got.Payload, payload) {
		t.Errorf("payload mutated in passthrough: got %q want %q", got.Payload, payload)
	}
}

func TestPassthroughWithEmptyRuleSet(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t)) // installed but empty

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))
	if got := disp.count(); got != 1 {
		t.Errorf("delivered = %d, want 1 (empty rule set must not drop)", got)
	}
}

func TestDropMatchingSegments(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Match:  Match{Port: 8080},
	}))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))

	other := defaultPkt()
	other.dstPort = 9999
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(other))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d packets, want 1 (only the non-matching one)", len(views))
	}
	if views[0].DstPort != 9999 {
		t.Errorf("wrong packet survived: dstPort=%d, want 9999", views[0].DstPort)
	}
}

// TestDropIsSilent is the headline capability: a dropped packet produces no
// RST. Today's proxy `drop` closes the connection, which sends a RST that
// clients handle correctly on the first try — so the bug class that actually
// takes production down (a socket stuck in ESTABLISHED writing into a void)
// is exactly the one Faultbox cannot currently reproduce.
func TestDropIsSilent(t *testing.T) {
	fe, disp, lower := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop}))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))

	if got := disp.count(); got != 0 {
		t.Errorf("packet was delivered despite drop rule (%d)", got)
	}
	for _, v := range lower.views() {
		if v.HasFlags(FlagRST) {
			t.Fatalf("drop emitted a RST (%s) — half-open blackhole is unreachable", v.String())
		}
	}
	if lower.count() != 0 {
		t.Errorf("drop produced %d outbound packets, want 0", lower.count())
	}
}

func TestDirectionMatching(t *testing.T) {
	fe, disp, lower := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Match:  Match{Dir: dirPtr(DirC2S)},
	}))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt())) // c2s → dropped
	writeOne(fe, makePacket(defaultPkt()))                                       // s2c → survives

	if got := disp.count(); got != 0 {
		t.Errorf("c2s packet delivered = %d, want 0", got)
	}
	if got := lower.count(); got != 1 {
		t.Errorf("s2c packet written = %d, want 1 (c2s rule must not fire on s2c)", got)
	}
}

func TestFlagMatching(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		flags    uint8
		wantDrop bool
	}{
		{"syn matches syn", "SYN", FlagSYN, true},
		{"syn does not match ack", "SYN", FlagACK, false},
		{"psh,ack matches both set", "PSH,ACK", FlagPSH | FlagACK, true},
		{"psh,ack needs both", "PSH,ACK", FlagACK, false},
		{"negation excludes rst", "!RST", FlagACK, true},
		{"negation blocks rst", "!RST", FlagRST, false},
		{"mixed set and clear", "ACK,!SYN", FlagACK, true},
		{"mixed rejects syn", "ACK,!SYN", FlagACK | FlagSYN, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, clear, err := ParseFlagSpec(tc.spec)
			if err != nil {
				t.Fatalf("ParseFlagSpec(%q): %v", tc.spec, err)
			}
			fe, disp, _ := newTestEndpoint(t, Options{})
			fe.SetRules(mustRules(t, &Rule{
				Action: ActionDrop,
				Match:  Match{FlagsSet: set, FlagsClear: clear},
			}))

			o := defaultPkt()
			o.flags = tc.flags
			fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

			dropped := disp.count() == 0
			if dropped != tc.wantDrop {
				t.Errorf("spec %q flags %s: dropped=%v, want %v",
					tc.spec, FlagNames(tc.flags), dropped, tc.wantDrop)
			}
		})
	}
}

func TestPayloadPrefixAndContains(t *testing.T) {
	tests := []struct {
		name     string
		match    Match
		payload  string
		wantDrop bool
	}{
		{"prefix hit", Match{PayloadPrefix: []byte("GET ")}, "GET /x HTTP/1.1", true},
		{"prefix miss", Match{PayloadPrefix: []byte("GET ")}, "POST /x", false},
		{"prefix longer than payload", Match{PayloadPrefix: []byte("GETGETGET")}, "GET", false},
		{"contains hit", Match{PayloadContains: []byte("secret")}, "a secret value", true},
		{"contains miss", Match{PayloadContains: []byte("secret")}, "nothing here", false},
		{"empty payload with prefix", Match{PayloadPrefix: []byte("x")}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe, disp, _ := newTestEndpoint(t, Options{})
			fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Match: tc.match}))

			o := defaultPkt()
			o.payload = []byte(tc.payload)
			fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

			dropped := disp.count() == 0
			if dropped != tc.wantDrop {
				t.Errorf("payload %q: dropped=%v, want %v", tc.payload, dropped, tc.wantDrop)
			}
		})
	}
}

func TestLengthMatching(t *testing.T) {
	tests := []struct {
		name     string
		match    Match
		size     int
		wantDrop bool
	}{
		{"len_gt fires above", Match{LenGT: 100, HasLenGT: true}, 200, true},
		{"len_gt excludes equal", Match{LenGT: 100, HasLenGT: true}, 100, false},
		{"len_lt fires below", Match{LenLT: 100, HasLenLT: true}, 50, true},
		{"len_lt excludes equal", Match{LenLT: 100, HasLenLT: true}, 100, false},
		{"exact len", Match{Len: 64, HasLen: true}, 64, true},
		{"exact len miss", Match{Len: 64, HasLen: true}, 65, false},
		{"exact zero len matches bare ack", Match{Len: 0, HasLen: true}, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe, disp, _ := newTestEndpoint(t, Options{})
			fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Match: tc.match}))

			o := defaultPkt()
			o.payload = make([]byte, tc.size)
			fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

			if dropped := disp.count() == 0; dropped != tc.wantDrop {
				t.Errorf("size %d: dropped=%v, want %v", tc.size, dropped, tc.wantDrop)
			}
		})
	}
}

func TestOccurrenceSelectors(t *testing.T) {
	tests := []struct {
		name    string
		trigger Trigger
		n       int
		total   int
		// wantDelivered is which 1-based packet ordinals survive a drop rule.
		wantDelivered []int
	}{
		{"nth fires once", TriggerNth, 3, 5, []int{1, 2, 4, 5}},
		{"after skips first n", TriggerAfter, 2, 5, []int{1, 2}},
		{"every third", TriggerEvery, 3, 7, []int{1, 2, 4, 5, 7}},
		{"always", TriggerAlways, 0, 3, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe, disp, _ := newTestEndpoint(t, Options{})
			fe.SetRules(mustRules(t, &Rule{
				Action:   ActionDrop,
				Trigger:  tc.trigger,
				TriggerN: tc.n,
			}))

			for i := 0; i < tc.total; i++ {
				o := defaultPkt()
				o.seq = uint32(1000 + i)
				fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))
			}

			got := disp.views()
			if len(got) != len(tc.wantDelivered) {
				t.Fatalf("delivered %d packets, want %d (%v)", len(got), len(tc.wantDelivered), tc.wantDelivered)
			}
			for i, ord := range tc.wantDelivered {
				wantSeq := uint32(1000 + ord - 1)
				if got[i].Seq != wantSeq {
					t.Errorf("delivered[%d].Seq = %d, want %d (packet #%d)", i, got[i].Seq, wantSeq, ord)
				}
			}
		})
	}
}

// TestOccurrenceCountersArePerFlow guards against a global counter, which
// would make `every=3` mean "every third packet anywhere" instead of "every
// third packet of this conversation".
func TestOccurrenceCountersArePerFlow(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop, Trigger: TriggerNth, TriggerN: 2}))

	// Two distinct flows, alternating. The rule counter is per-rule (matching
	// the syscall-fault semantics), but PacketView.Index must be per-flow.
	a := defaultPkt()
	b := defaultPkt()
	b.srcPort = 41000

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(a))
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(b))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d, want 1", len(views))
	}
	if views[0].Index != 0 {
		t.Errorf("surviving packet Index = %d, want 0 (per-flow ordinal)", views[0].Index)
	}
	if views[0].Flow == "" {
		t.Error("Flow is empty; occurrence selectors cannot be scoped")
	}
}

func TestFlowIDIsDirectionIndependent(t *testing.T) {
	out := defaultPkt()
	in := pktOpts{
		srcIP: out.dstIP, dstIP: out.srcIP,
		srcPort: out.dstPort, dstPort: out.srcPort,
		flags: FlagACK,
	}
	pvOut, ok := Parse(makePacket(out), DirC2S)
	if !ok {
		t.Fatal("parse outbound")
	}
	pvIn, ok := Parse(makePacket(in), DirS2C)
	if !ok {
		t.Fatal("parse inbound")
	}
	if pvOut.Flow != pvIn.Flow {
		t.Errorf("flow differs by direction:\n  c2s=%s\n  s2c=%s", pvOut.Flow, pvIn.Flow)
	}
}

func TestRuleOrderFirstMatchWins(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	// A narrow pass rule ahead of a broad drop: the classic exception carve-out.
	fe.SetRules(mustRules(t,
		&Rule{Action: ActionPass, Match: Match{Port: 8080, PayloadPrefix: []byte("KEEP")}},
		&Rule{Action: ActionDrop, Match: Match{Port: 8080}},
	))

	keep := defaultPkt()
	keep.payload = []byte("KEEP me")
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(keep))

	drop := defaultPkt()
	drop.payload = []byte("drop me")
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(drop))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d, want 1", len(views))
	}
	if !bytes.HasPrefix(views[0].Payload, []byte("KEEP")) {
		t.Errorf("wrong packet survived: %q", views[0].Payload)
	}
}

func TestDuplicateDeliversTwice(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action:         ActionDuplicate,
		DuplicateCount: 2,
		Match:          Match{Proto: ProtoUDP},
	}))

	o := defaultPkt()
	o.udp = true
	o.payload = []byte("metric:1|c")
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	views := disp.views()
	if len(views) != 2 {
		t.Fatalf("delivered %d copies, want 2", len(views))
	}
	for i, v := range views {
		if !bytes.Equal(v.Payload, []byte("metric:1|c")) {
			t.Errorf("copy %d payload = %q, want the original", i, v.Payload)
		}
	}
}

func TestCorruptFlipMutatesPayload(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action:        ActionCorrupt,
		CorruptOffset: 0,
		CorruptLength: 2,
		CorruptMode:   CorruptFlip,
		Checksum:      ChecksumFix,
	}))

	o := defaultPkt()
	o.payload = []byte{0x00, 0x00, 0xAA}
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d, want 1", len(views))
	}
	got := views[0].Payload
	want := []byte{0xFF, 0xFF, 0xAA}
	if !bytes.Equal(got, want) {
		t.Errorf("corrupt flip = %v, want %v", got, want)
	}
}

func TestCorruptZeroAndBounds(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	// Length runs past the end of the payload — must clamp, not panic.
	fe.SetRules(mustRules(t, &Rule{
		Action:        ActionCorrupt,
		CorruptOffset: 1,
		CorruptLength: 100,
		CorruptMode:   CorruptZero,
		Checksum:      ChecksumFix,
	}))

	o := defaultPkt()
	o.payload = []byte{0xAA, 0xBB, 0xCC}
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d, want 1", len(views))
	}
	want := []byte{0xAA, 0x00, 0x00}
	if !bytes.Equal(views[0].Payload, want) {
		t.Errorf("corrupt zero = %v, want %v", views[0].Payload, want)
	}
}

// TestCorruptChecksumPolicy pins the difference between the two modes: "fix"
// yields a packet a receiver accepts with wrong data (silent corruption
// reaching the application); "break" yields one the receiver's stack discards
// (loss that looks like congestion).
func TestCorruptChecksumPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy    ChecksumPolicy
		wantValid bool
	}{
		{ChecksumFix, true},
		{ChecksumBreak, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			fe, _, lower := newTestEndpoint(t, Options{})
			fe.SetRules(mustRules(t, &Rule{
				Action:        ActionCorrupt,
				CorruptLength: 4,
				CorruptMode:   CorruptFlip,
				Checksum:      tc.policy,
			}))

			o := defaultPkt()
			o.payload = []byte("payloadpayload")
			writeOne(fe, makePacket(o))

			if lower.count() != 1 {
				t.Fatalf("written %d, want 1", lower.count())
			}
			if got := tcpChecksumValid(t, lower); got != tc.wantValid {
				t.Errorf("checksum valid = %v, want %v for policy %q", got, tc.wantValid, tc.policy)
			}
		})
	}
}

func TestWindowRewrite(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{
		Action:     ActionWindow,
		WindowSize: 0,
		Match:      Match{Proto: ProtoTCP},
	}))

	o := defaultPkt()
	o.window = 65535
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	views := disp.views()
	if len(views) != 1 {
		t.Fatalf("delivered %d, want 1", len(views))
	}
	if views[0].Window != 0 {
		t.Errorf("window = %d, want 0 (zero-window stall)", views[0].Window)
	}
}

// TestResetSynthesizesRST drives an ingress packet, so the RST must come back
// out the *wire* toward the sender — not up into the local stack, which would
// reset the wrong side of the connection.
func TestResetSynthesizesRST(t *testing.T) {
	fe, disp, lower := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionReset, Match: Match{Proto: ProtoTCP}}))

	o := defaultPkt()
	o.seq = 5000
	o.ack = 9000
	o.payload = []byte("0123456789") // 10 bytes
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	if got := disp.count(); got != 0 {
		t.Errorf("original packet was delivered up the stack (%d); reset must drop it", got)
	}
	views := lower.views()
	if len(views) != 1 {
		t.Fatalf("wrote %d packets to the wire, want 1 (the RST travelling back to the sender)", len(views))
	}
	rst := views[0]
	if !rst.HasFlags(FlagRST) {
		t.Fatalf("packet is not a RST: %s", rst.String())
	}
	// RFC 793: a RST answering a data segment acknowledges it, or the peer
	// discards the reset as out of window.
	if want := uint32(5000 + 10); rst.Ack != want {
		t.Errorf("RST ack = %d, want %d (seq + payload len)", rst.Ack, want)
	}
	if rst.Seq != 9000 {
		t.Errorf("RST seq = %d, want 9000 (the original's ack)", rst.Seq)
	}
	// Ports must be swapped — the RST travels back toward the sender.
	if rst.SrcPort != o.dstPort || rst.DstPort != o.srcPort {
		t.Errorf("RST ports = %d->%d, want %d->%d",
			rst.SrcPort, rst.DstPort, o.dstPort, o.srcPort)
	}
}

func TestResetOnSynAcksSynPlusOne(t *testing.T) {
	fe, _, lower := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionReset}))

	o := defaultPkt()
	o.flags = FlagSYN
	o.seq = 700
	o.payload = nil
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	views := lower.views()
	if len(views) != 1 {
		t.Fatalf("wrote %d packets, want 1 (the RST)", len(views))
	}
	if want := uint32(701); views[0].Ack != want {
		t.Errorf("RST ack for bare SYN = %d, want %d", views[0].Ack, want)
	}
}

func TestUnparseablePacketPassesThrough(t *testing.T) {
	fe, disp, _ := newTestEndpoint(t, Options{})
	fe.SetRules(mustRules(t, &Rule{Action: ActionDrop}))

	// A runt that is not a valid IPv4 packet. A rule cannot meaningfully
	// match what we could not parse, so it must survive rather than vanish.
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: makeRawBuffer([]byte{0x01, 0x02, 0x03}),
	})
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)

	if got := disp.count(); got != 1 {
		t.Errorf("unparseable packet delivered = %d, want 1 (pass through, don't drop)", got)
	}
}

func TestMatchCountAndFireCount(t *testing.T) {
	fe, _, _ := newTestEndpoint(t, Options{})
	r := &Rule{Action: ActionDrop, Trigger: TriggerNth, TriggerN: 2}
	fe.SetRules(mustRules(t, r))

	for i := 0; i < 5; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))
	}
	if got := r.MatchCount(); got != 5 {
		t.Errorf("MatchCount = %d, want 5", got)
	}
	if got := r.FireCount(); got != 1 {
		t.Errorf("FireCount = %d, want 1 (nth=2 fires once)", got)
	}
}

// TestNoMatchLeavesZeroCount backs the "no injections fired" diagnostic: a
// rule that never matched must be distinguishable from one that matched and
// chose not to fire.
func TestNoMatchLeavesZeroCount(t *testing.T) {
	fe, _, _ := newTestEndpoint(t, Options{})
	r := &Rule{Action: ActionDrop, Match: Match{Port: 12345}}
	fe.SetRules(mustRules(t, r))

	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))

	if got := r.MatchCount(); got != 0 {
		t.Errorf("MatchCount = %d, want 0 for a rule that never matched", got)
	}
}

func TestEventsEmitted(t *testing.T) {
	var events []Event
	fe, _, _ := newTestEndpoint(t, Options{
		OnEvent: func(e Event) { events = append(events, e) },
	})
	fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Label:  "blackhole",
		Match:  Match{Dir: dirPtr(DirC2S)},
	}))

	o := defaultPkt()
	o.flags = FlagPSH | FlagACK
	o.payload = []byte("hi")
	fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(o))

	if len(events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(events))
	}
	e := events[0]
	if e.Action != ActionDrop {
		t.Errorf("Action = %s, want drop", e.Action)
	}
	if e.Direction != DirC2S {
		t.Errorf("Direction = %s, want c2s", e.Direction)
	}
	if e.Protocol != ProtoTCP {
		t.Errorf("Protocol = %s, want tcp", e.Protocol)
	}
	if e.RuleLabel != "blackhole" {
		t.Errorf("RuleLabel = %q, want %q", e.RuleLabel, "blackhole")
	}
	if e.Len != 2 {
		t.Errorf("Len = %d, want 2", e.Len)
	}
	if e.Flow == "" {
		t.Error("Flow is empty")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// tcpChecksumValid recomputes the TCP checksum over the last packet the lower
// endpoint saw. A correct segment sums to 0xFFFF including its own checksum
// field, which avoids depending on any particular gVisor validation helper.
func tcpChecksumValid(t *testing.T, lower *recordEndpoint) bool {
	t.Helper()
	lower.mu.Lock()
	defer lower.mu.Unlock()
	if len(lower.raw) == 0 {
		t.Fatal("no raw packet recorded")
	}
	b := lower.raw[len(lower.raw)-1]
	ip := header.IPv4(b)
	rest := b[ip.HeaderLength():]
	xsum := header.PseudoHeaderChecksum(
		header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(rest)))
	return checksum.Checksum(rest, xsum) == 0xFFFF
}
