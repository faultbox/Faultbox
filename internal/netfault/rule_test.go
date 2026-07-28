package netfault

import (
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestRuleValidation(t *testing.T) {
	// Rule carries atomic counters, so the table holds *Rule — copying one by
	// value trips `go vet`'s copylocks check.
	tests := []struct {
		name    string
		rule    *Rule
		wantErr string // substring; empty means valid
	}{
		{"drop needs nothing", &Rule{Action: ActionDrop}, ""},
		{"delay needs a duration", &Rule{Action: ActionDelay}, "duration must be positive"},
		{"delay with duration", &Rule{Action: ActionDelay, Delay: time.Second}, ""},
		{"negative delay", &Rule{Action: ActionDelay, Delay: -time.Second}, "duration must be positive"},

		{"reorder needs by", &Rule{Action: ActionReorder}, "by must be >= 1"},
		{"reorder valid", &Rule{Action: ActionReorder, ReorderBy: 2}, ""},

		{"duplicate needs >= 2", &Rule{Action: ActionDuplicate, DuplicateCount: 1}, "count must be >= 2"},
		{"duplicate valid", &Rule{Action: ActionDuplicate, DuplicateCount: 2}, ""},

		{"corrupt needs length", &Rule{Action: ActionCorrupt, CorruptMode: CorruptFlip, Checksum: ChecksumFix}, "length must be >= 1"},
		{"corrupt negative offset", &Rule{Action: ActionCorrupt, CorruptLength: 1, CorruptOffset: -1, CorruptMode: CorruptFlip, Checksum: ChecksumFix}, "offset must be >= 0"},
		{"corrupt bad mode", &Rule{Action: ActionCorrupt, CorruptLength: 1, CorruptMode: "scramble", Checksum: ChecksumFix}, "mode must be one of"},
		{"corrupt bad checksum", &Rule{Action: ActionCorrupt, CorruptLength: 1, CorruptMode: CorruptFlip, Checksum: "maybe"}, "checksum must be fix or break"},
		{"corrupt valid", &Rule{Action: ActionCorrupt, CorruptLength: 4, CorruptMode: CorruptFlip, Checksum: ChecksumFix}, ""},

		{"reset on udp rejected", &Rule{Action: ActionReset, Match: Match{Proto: ProtoUDP}}, "only meaningful for tcp"},
		{"reset on tcp ok", &Rule{Action: ActionReset, Match: Match{Proto: ProtoTCP}}, ""},
		{"window on udp rejected", &Rule{Action: ActionWindow, Match: Match{Proto: ProtoUDP}}, "only meaningful for tcp"},

		{"probability above 1", &Rule{Action: ActionDrop, Probability: 1.5}, "within 0..1"},
		{"probability below 0", &Rule{Action: ActionDrop, Probability: -0.1}, "within 0..1"},

		{"max_fires needs partial probability", &Rule{Action: ActionDrop, Probability: 1, MaxFires: 3}, "0 < probability < 1"},
		{"max_fires with unset probability", &Rule{Action: ActionDrop, MaxFires: 3}, "0 < probability < 1"},
		{"max_fires valid", &Rule{Action: ActionDrop, Probability: 0.3, MaxFires: 3}, ""},
		{"stochastic + max_fires", &Rule{Action: ActionDrop, Probability: 0.3, MaxFires: 2, Mode: "stochastic"}, "incompatible with max_fires"},

		{"trigger needs count", &Rule{Action: ActionDrop, Trigger: TriggerNth}, "requires a positive count"},
		{"trigger valid", &Rule{Action: ActionDrop, Trigger: TriggerNth, TriggerN: 2}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewRuleSetReportsIndex(t *testing.T) {
	_, err := NewRuleSet(
		&Rule{Action: ActionDrop},
		&Rule{Action: ActionDelay}, // invalid: no duration
	)
	if err == nil {
		t.Fatal("NewRuleSet accepted an invalid rule")
	}
	if !strings.Contains(err.Error(), "rule 1") {
		t.Errorf("error does not identify which rule failed: %v", err)
	}
}

func TestParseFlagSpec(t *testing.T) {
	tests := []struct {
		spec      string
		set       uint8
		clear     uint8
		wantError string
	}{
		{"", 0, 0, ""},
		{"SYN", FlagSYN, 0, ""},
		{"syn", FlagSYN, 0, ""}, // case insensitive
		{"PSH,ACK", FlagPSH | FlagACK, 0, ""},
		{" PSH , ACK ", FlagPSH | FlagACK, 0, ""}, // whitespace tolerant
		{"!RST", 0, FlagRST, ""},
		{"ACK,!SYN", FlagACK, FlagSYN, ""},
		{"NOPE", 0, 0, "unknown TCP flag"},
		{"SYN,!SYN", 0, 0, "both requires and forbids"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			set, clear, err := ParseFlagSpec(tc.spec)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("ParseFlagSpec(%q) error = %v, want %q", tc.spec, err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFlagSpec(%q): %v", tc.spec, err)
			}
			if set != tc.set || clear != tc.clear {
				t.Errorf("ParseFlagSpec(%q) = set %v clear %v, want set %v clear %v",
					tc.spec, FlagNames(set), FlagNames(clear), FlagNames(tc.set), FlagNames(tc.clear))
			}
		})
	}
}

func TestFlagNamesStableOrder(t *testing.T) {
	got := FlagNames(FlagACK | FlagSYN | FlagPSH)
	want := []string{"SYN", "PSH", "ACK"} // TCP header bit order, not input order
	if len(got) != len(want) {
		t.Fatalf("FlagNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FlagNames = %v, want %v (order must be stable for golden output)", got, want)
		}
	}
}

func TestParseDirection(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Direction
		ok   bool
	}{
		{"c2s", DirC2S, true},
		{"s2c", DirS2C, true},
		{"both", 0, false}, // not a direction; the absence of one
		{"", 0, false},
		{"nonsense", 0, false},
	} {
		got, ok := ParseDirection(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ParseDirection(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPayloadCapIsBounded guards the datapath against copying a jumbo frame
// into every matcher evaluation.
func TestPayloadCapIsBounded(t *testing.T) {
	o := defaultPkt()
	o.payload = make([]byte, maxPayloadCapture+2048)
	pv, ok := Parse(makePacket(o), DirC2S)
	if !ok {
		t.Fatal("parse failed")
	}
	if len(pv.Payload) != maxPayloadCapture {
		t.Errorf("captured payload = %d bytes, want the %d cap", len(pv.Payload), maxPayloadCapture)
	}
	// PayloadLen must still report the true size, or len_gt matching lies.
	if pv.PayloadLen != maxPayloadCapture+2048 {
		t.Errorf("PayloadLen = %d, want the true length %d", pv.PayloadLen, maxPayloadCapture+2048)
	}
}

func TestMatchStringRendersPredicates(t *testing.T) {
	m := Match{
		Dir:      dirPtr(DirC2S),
		Proto:    ProtoTCP,
		FlagsSet: FlagPSH | FlagACK,
		Port:     5432,
		LenGT:    1400, HasLenGT: true,
	}
	got := m.String()
	for _, want := range []string{"dir=c2s", "proto=tcp", "flags=PSH,ACK", "port=5432", "len>1400"} {
		if !strings.Contains(got, want) {
			t.Errorf("Match.String() = %q, missing %q", got, want)
		}
	}
	if (&Match{}).String() != "*" {
		t.Errorf("empty Match renders as %q, want \"*\"", (&Match{}).String())
	}
}

// ─── benchmarks ────────────────────────────────────────────────────────────
//
// Reported in the milestone commit alongside the RFC-024 proxy numbers, and
// the input to RFC-054 open questions 3 and 4 (payload cap, where= budget).

func BenchmarkPassthroughNoRules(b *testing.B) {
	lower := &recordEndpoint{}
	fe := New(lower, Options{})
	fe.Attach(&nopDispatcher{})
	defer fe.Close()

	pkt := makePacket(withPayload(1024))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	}
}

func BenchmarkDeclarativeMatch(b *testing.B) {
	lower := &recordEndpoint{}
	fe := New(lower, Options{})
	fe.Attach(&nopDispatcher{})
	defer fe.Close()

	rs, _ := NewRuleSet(&Rule{
		Action: ActionPass,
		Match: Match{
			Dir: dirPtr(DirC2S), Proto: ProtoTCP,
			FlagsSet: FlagACK, Port: 8080,
			LenGT: 100, HasLenGT: true,
		},
	})
	fe.SetRules(rs)

	pkt := makePacket(withPayload(1024))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	}
}

// BenchmarkWherePredicate measures the *plumbing* cost of the escape hatch
// with a Go closure standing in for the lambda. It deliberately does NOT
// measure Starlark: a real starlark.Call takes a thread lock and allocates,
// and is expected to dominate this number by orders of magnitude. Do not cite
// this benchmark as evidence that `where=` is cheap — the honest measurement
// lands in M2 once the Starlark bridge exists, and it is the input to RFC-054
// open question 4 (warn vs fail on a hot lambda).
func BenchmarkWherePredicate(b *testing.B) {
	lower := &recordEndpoint{}
	fe := New(lower, Options{})
	fe.Attach(&nopDispatcher{})
	defer fe.Close()

	rs, _ := NewRuleSet(&Rule{
		Action: ActionPass,
		Match:  Match{Where: func(p *PacketView) bool { return p.PayloadLen > 100 }},
	})
	fe.SetRules(rs)

	pkt := makePacket(withPayload(1024))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	}
}

func BenchmarkDropRule(b *testing.B) {
	lower := &recordEndpoint{}
	fe := New(lower, Options{})
	fe.Attach(&nopDispatcher{})
	defer fe.Close()

	rs, _ := NewRuleSet(&Rule{Action: ActionDrop, Match: Match{Port: 8080}})
	fe.SetRules(rs)

	pkt := makePacket(withPayload(1024))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	}
}

func BenchmarkParse(b *testing.B) {
	pkt := makePacket(withPayload(1024))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Parse(pkt, DirC2S)
	}
}

func withPayload(n int) pktOpts {
	o := defaultPkt()
	o.payload = make([]byte, n)
	return o
}

// nopDispatcher discards deliveries so a benchmark measures the rule pipeline
// rather than the recording harness.
type nopDispatcher struct{}

func (nopDispatcher) DeliverNetworkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}
func (nopDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer)    {}
