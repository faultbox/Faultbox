package netfault

import (
	"bytes"
	"fmt"
	"strings"
)

// Match is the declarative predicate a spec author writes as kwargs on a
// packet_* builtin. Every field is optional; the set is ANDed. A zero Match
// matches every packet.
//
// The whole point of keeping this declarative is cost: these predicates
// evaluate in Go with no allocation and no lock, on the datapath, per packet.
// The Starlark escape hatch (Where) is only consulted for packets that already
// passed everything here, so a lambda refines a cheap filter rather than
// replacing it.
type Match struct {
	// Dir constrains direction. nil means either.
	Dir *Direction
	// Proto constrains the transport. Empty means any.
	Proto Protocol

	// FlagsSet requires every listed bit to be present; FlagsClear requires
	// every listed bit to be absent (spec spelling "!RST").
	FlagsSet   uint8
	FlagsClear uint8

	// Port matches the destination port. Zero means any.
	Port uint16

	// DstIP matches the destination address. Empty means any.
	//
	// Used to scope a rule to one (consumer, service, interface) triple: the
	// gateway allocates a distinct address per triple, so the destination
	// identifies the sender even though Docker masquerades the source IP.
	DstIP string

	// Payload-length bounds. LenGT/LenLT are exclusive; Len is exact.
	// The Has* flags distinguish "unset" from "zero", which matters because a
	// zero-length payload is a real and interesting case (a bare ACK).
	Len      int
	HasLen   bool
	LenGT    int
	HasLenGT bool
	LenLT    int
	HasLenLT bool

	// PayloadPrefix / PayloadContains match raw bytes. Bounded by the
	// 4 KiB capture cap in PacketView.
	PayloadPrefix   []byte
	PayloadContains []byte

	// Where is the Starlark escape hatch, evaluated last and only if every
	// declarative predicate above already passed. nil when unused.
	//
	// The engine cannot verify that a Where predicate is pure. An impure
	// lambda silently breaks the L1 replay contract; that is documented, not
	// detected.
	Where func(*PacketView) bool
}

// Matches evaluates the predicate against a packet.
//
// onWhereEval, when non-nil, is invoked immediately before each Starlark
// predicate call. It is the only accurate way to count how often the
// expensive path is really taken — counting rules that merely *carry* a
// lambda would overstate cost for every packet the cheap predicates already
// excluded, which is exactly the population a `where=` refinement is supposed
// to avoid paying for.
func (m *Match) Matches(p *PacketView, onWhereEval func()) bool {
	if !m.matchesDeclarative(p) {
		return false
	}
	if m.Where != nil {
		if onWhereEval != nil {
			onWhereEval()
		}
		return m.Where(p)
	}
	return true
}

// matchesDeclarative evaluates everything except Where. Split out so the
// endpoint can count how often the expensive Starlark path is actually
// reached, and so tests can assert that a lambda is never called for a packet
// the cheap predicates already excluded.
func (m *Match) matchesDeclarative(p *PacketView) bool {
	if m.Dir != nil && *m.Dir != p.Dir {
		return false
	}
	if m.Proto != "" && m.Proto != p.Proto {
		return false
	}
	if m.FlagsSet != 0 && p.Flags&m.FlagsSet != m.FlagsSet {
		return false
	}
	if m.FlagsClear != 0 && p.Flags&m.FlagsClear != 0 {
		return false
	}
	if m.Port != 0 && p.DstPort != m.Port {
		return false
	}
	if m.DstIP != "" && p.DstIP != m.DstIP {
		return false
	}
	if m.HasLen && p.PayloadLen != m.Len {
		return false
	}
	if m.HasLenGT && p.PayloadLen <= m.LenGT {
		return false
	}
	if m.HasLenLT && p.PayloadLen >= m.LenLT {
		return false
	}
	if len(m.PayloadPrefix) > 0 && !bytes.HasPrefix(p.Payload, m.PayloadPrefix) {
		return false
	}
	if len(m.PayloadContains) > 0 && !bytes.Contains(p.Payload, m.PayloadContains) {
		return false
	}
	return true
}

// UsesWhere reports whether this matcher carries a Starlark predicate.
func (m *Match) UsesWhere() bool { return m.Where != nil }

// ParseFlagSpec turns a spec-level flag string into set/clear masks.
//
//	"SYN"       → set SYN
//	"PSH,ACK"   → set PSH and ACK
//	"!RST"      → clear RST
//	"ACK,!SYN"  → set ACK, clear SYN
//
// An unknown or repeated-with-conflict flag is an error rather than a silent
// no-op — v0.13.x established that silently-dropped matcher kwargs are a bug
// class worth failing loudly on.
func ParseFlagSpec(spec string) (set, clear uint8, err error) {
	if strings.TrimSpace(spec) == "" {
		return 0, 0, nil
	}
	for _, part := range strings.Split(spec, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		negate := false
		if strings.HasPrefix(p, "!") {
			negate = true
			p = strings.TrimSpace(p[1:])
		}
		bit, ok := ParseFlagName(p)
		if !ok {
			return 0, 0, fmt.Errorf("unknown TCP flag %q (known: FIN, SYN, RST, PSH, ACK, URG, ECE, CWR)", p)
		}
		if negate {
			clear |= bit
		} else {
			set |= bit
		}
	}
	if set&clear != 0 {
		return 0, 0, fmt.Errorf("flag spec %q both requires and forbids the same flag", spec)
	}
	return set, clear, nil
}

// String renders the matcher for diagnostics and event labels.
func (m *Match) String() string {
	var parts []string
	if m.Dir != nil {
		parts = append(parts, "dir="+m.Dir.String())
	}
	if m.Proto != "" {
		parts = append(parts, "proto="+string(m.Proto))
	}
	if m.FlagsSet != 0 {
		parts = append(parts, "flags="+strings.Join(FlagNames(m.FlagsSet), ","))
	}
	if m.FlagsClear != 0 {
		parts = append(parts, "!flags="+strings.Join(FlagNames(m.FlagsClear), ","))
	}
	if m.Port != 0 {
		parts = append(parts, fmt.Sprintf("port=%d", m.Port))
	}
	if m.DstIP != "" {
		parts = append(parts, "dst="+m.DstIP)
	}
	if m.HasLen {
		parts = append(parts, fmt.Sprintf("len=%d", m.Len))
	}
	if m.HasLenGT {
		parts = append(parts, fmt.Sprintf("len>%d", m.LenGT))
	}
	if m.HasLenLT {
		parts = append(parts, fmt.Sprintf("len<%d", m.LenLT))
	}
	if len(m.PayloadPrefix) > 0 {
		parts = append(parts, fmt.Sprintf("prefix=%q", m.PayloadPrefix))
	}
	if len(m.PayloadContains) > 0 {
		parts = append(parts, fmt.Sprintf("contains=%q", m.PayloadContains))
	}
	if m.Where != nil {
		parts = append(parts, "where=<lambda>")
	}
	if len(parts) == 0 {
		return "*"
	}
	return strings.Join(parts, " ")
}
