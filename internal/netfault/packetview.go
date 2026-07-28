package netfault

import (
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Direction records which way a packet is travelling relative to the SUT.
//
// The gateway terminates the connection the SUT dials, so netstack is always
// the server side: packets the SUT sends arrive as ingress, packets netstack
// sends to the SUT leave as egress.
type Direction int

const (
	// DirC2S is SUT → Faultbox (ingress into netstack).
	DirC2S Direction = iota
	// DirS2C is Faultbox → SUT (egress out of netstack).
	DirS2C
)

func (d Direction) String() string {
	if d == DirC2S {
		return "c2s"
	}
	return "s2c"
}

// ParseDirection maps the spec-facing spelling onto a Direction. The bool
// reports whether the input was recognised; "both" is not a Direction (it is
// the absence of a direction predicate) and is rejected here on purpose so
// callers handle it explicitly.
func ParseDirection(s string) (Direction, bool) {
	switch s {
	case "c2s":
		return DirC2S, true
	case "s2c":
		return DirS2C, true
	}
	return 0, false
}

// TCP flag bits, re-exported so callers do not need the gVisor header package.
const (
	FlagFIN = uint8(header.TCPFlagFin)
	FlagSYN = uint8(header.TCPFlagSyn)
	FlagRST = uint8(header.TCPFlagRst)
	FlagPSH = uint8(header.TCPFlagPsh)
	FlagACK = uint8(header.TCPFlagAck)
	FlagURG = uint8(header.TCPFlagUrg)
	FlagECE = uint8(header.TCPFlagEce)
	FlagCWR = uint8(header.TCPFlagCwr)
)

var flagNames = []struct {
	bit  uint8
	name string
}{
	{FlagFIN, "FIN"},
	{FlagSYN, "SYN"},
	{FlagRST, "RST"},
	{FlagPSH, "PSH"},
	{FlagACK, "ACK"},
	{FlagURG, "URG"},
	{FlagECE, "ECE"},
	{FlagCWR, "CWR"},
}

// ParseFlagName maps a flag spelling ("SYN", "ack") to its bit. Case
// insensitive so specs can be written either way.
func ParseFlagName(s string) (uint8, bool) {
	up := strings.ToUpper(strings.TrimSpace(s))
	for _, f := range flagNames {
		if f.name == up {
			return f.bit, true
		}
	}
	return 0, false
}

// FlagNames renders a flag bitmask as a stable, ordered list. Order follows
// the TCP header bit order so two packets with the same flags always render
// identically — the event log and golden tests depend on that.
func FlagNames(flags uint8) []string {
	var out []string
	for _, f := range flagNames {
		if flags&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return out
}

// Protocol identifies the transport carried by a packet.
type Protocol string

const (
	ProtoTCP   Protocol = "tcp"
	ProtoUDP   Protocol = "udp"
	ProtoICMP  Protocol = "icmp"
	ProtoOther Protocol = "other"
)

// PacketView is a parsed, read-only projection of one packet. It is the value
// every matcher predicate reads and the backing store for the Starlark Packet
// type that lands with the DSL.
//
// Payload aliases the packet's buffer rather than copying it — a matcher runs
// on the datapath and must not allocate per packet. Anything that outlives the
// call (an event field, a Starlark value) must copy first.
type PacketView struct {
	Proto Protocol
	Dir   Direction

	SrcIP   string
	DstIP   string
	SrcPort uint16
	DstPort uint16

	// PayloadLen is transport payload only — IP and TCP/UDP headers excluded.
	PayloadLen int
	Payload    []byte

	// TCP-only. Zero for every other protocol.
	Flags  uint8
	Seq    uint32
	Ack    uint32
	Window uint16

	// Index is this packet's 0-based ordinal within its flow, assigned by the
	// endpoint's flow table. Drives the nth / after / every selectors.
	Index int
	// Flow is a stable identifier for the connection, direction-independent
	// so both legs share one counter.
	Flow string
}

// HasFlags reports whether every bit in mask is set.
func (p *PacketView) HasFlags(mask uint8) bool { return p.Flags&mask == mask }

// String renders a one-line description used in traces and test failures.
func (p *PacketView) String() string {
	switch p.Proto {
	case ProtoTCP:
		return fmt.Sprintf("%s tcp %s:%d->%s:%d flags=%s seq=%d win=%d len=%d",
			p.Dir, p.SrcIP, p.SrcPort, p.DstIP, p.DstPort,
			strings.Join(FlagNames(p.Flags), "|"), p.Seq, p.Window, p.PayloadLen)
	case ProtoUDP:
		return fmt.Sprintf("%s udp %s:%d->%s:%d len=%d",
			p.Dir, p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, p.PayloadLen)
	default:
		return fmt.Sprintf("%s %s %s->%s len=%d", p.Dir, p.Proto, p.SrcIP, p.DstIP, p.PayloadLen)
	}
}

// PacketBytes returns the packet's wire bytes.
//
// aliased reports whether the returned slice points *into* the PacketBuffer.
// When it does, writing to the slice mutates the packet in place, which is how
// corrupt and window rewriting work. When it does not, the slice is a copy and
// any write would be silently discarded — so mutating callers must check this
// and bail loudly rather than appear to succeed.
//
// PacketBuffer.ToView() always allocates and copies (see its implementation),
// which makes it wrong for mutation and needlessly expensive for a per-packet
// datapath. AsSlices() hands back the backing views instead: the single-view
// case — every packet read from a TUN, and every packet the tests build — is
// both zero-copy and mutable.
func PacketBytes(pkt *stack.PacketBuffer) (b []byte, aliased bool) {
	slices := pkt.AsSlices()
	switch len(slices) {
	case 0:
		return nil, false
	case 1:
		return slices[0], true
	}
	// Fragmented across views: flatten for reading. Callers that intend to
	// mutate get aliased=false and must not proceed.
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	out := make([]byte, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out, false
}

// maxPayloadCapture bounds how much of a packet's payload the matcher can see.
// Rules that need more than this are asking for stream reassembly, which is
// what the L7 proxies are for.
const maxPayloadCapture = 4096

// Parse projects a PacketBuffer into a PacketView. ok is false when the packet
// is not IPv4 or is too short to carry a valid header — such packets are passed
// through unmodified rather than faulted, since a rule cannot meaningfully
// match something we could not parse.
//
// The gateway sees L3 frames (IFF_TUN has no ethernet header), so parsing
// starts at the IP header.
func Parse(pkt *stack.PacketBuffer, dir Direction) (PacketView, bool) {
	b, _ := PacketBytes(pkt)
	if len(b) < header.IPv4MinimumSize {
		return PacketView{}, false
	}
	ip := header.IPv4(b)
	if !ip.IsValid(len(b)) {
		return PacketView{}, false
	}
	hlen := int(ip.HeaderLength())
	if hlen < header.IPv4MinimumSize || hlen > len(b) {
		return PacketView{}, false
	}

	pv := PacketView{
		Dir:   dir,
		SrcIP: ip.SourceAddress().String(),
		DstIP: ip.DestinationAddress().String(),
	}
	rest := b[hlen:]

	switch ip.TransportProtocol() {
	case header.TCPProtocolNumber:
		if len(rest) < header.TCPMinimumSize {
			return PacketView{}, false
		}
		t := header.TCP(rest)
		off := int(t.DataOffset())
		if off < header.TCPMinimumSize || off > len(rest) {
			return PacketView{}, false
		}
		pv.Proto = ProtoTCP
		pv.SrcPort = t.SourcePort()
		pv.DstPort = t.DestinationPort()
		pv.Flags = uint8(t.Flags())
		pv.Seq = t.SequenceNumber()
		pv.Ack = t.AckNumber()
		pv.Window = t.WindowSize()
		pv.Payload = clampPayload(rest[off:])
		pv.PayloadLen = len(rest) - off

	case header.UDPProtocolNumber:
		if len(rest) < header.UDPMinimumSize {
			return PacketView{}, false
		}
		u := header.UDP(rest)
		pv.Proto = ProtoUDP
		pv.SrcPort = u.SourcePort()
		pv.DstPort = u.DestinationPort()
		pv.Payload = clampPayload(rest[header.UDPMinimumSize:])
		pv.PayloadLen = len(rest) - header.UDPMinimumSize

	case header.ICMPv4ProtocolNumber:
		pv.Proto = ProtoICMP
		pv.PayloadLen = len(rest)
		pv.Payload = clampPayload(rest)

	default:
		pv.Proto = ProtoOther
		pv.PayloadLen = len(rest)
		pv.Payload = clampPayload(rest)
	}

	pv.Flow = flowID(&pv)
	return pv, true
}

func clampPayload(b []byte) []byte {
	if len(b) > maxPayloadCapture {
		return b[:maxPayloadCapture]
	}
	return b
}

// flowID builds a direction-independent connection identifier by ordering the
// two endpoints, so a packet and its reply share one flow — and therefore one
// occurrence counter. A rule written as `every=3` counts segments of the
// conversation, not of one direction, which is what a spec author means.
//
// Built with a strings.Builder rather than fmt.Sprintf: this runs once per
// packet on the datapath, and the Sprintf version was the single largest cost
// in Parse (three allocations and three format walks per packet).
func flowID(p *PacketView) string {
	lo, loPort := p.SrcIP, p.SrcPort
	hi, hiPort := p.DstIP, p.DstPort
	if lo > hi || (lo == hi && loPort > hiPort) {
		lo, loPort, hi, hiPort = hi, hiPort, lo, loPort
	}
	var b strings.Builder
	b.Grow(len(p.Proto) + len(lo) + len(hi) + 16)
	b.WriteString(string(p.Proto))
	b.WriteByte('|')
	b.WriteString(lo)
	b.WriteByte(':')
	writeUint(&b, uint64(loPort))
	b.WriteByte('-')
	b.WriteString(hi)
	b.WriteByte(':')
	writeUint(&b, uint64(hiPort))
	return b.String()
}

func writeUint(b *strings.Builder, v uint64) {
	var tmp [20]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	b.Write(tmp[i:])
}
