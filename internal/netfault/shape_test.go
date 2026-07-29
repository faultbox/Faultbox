package netfault

import (
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64 // bytes per second
	}{
		// Bit units: the networking convention.
		{"1bit", 0.125},
		{"8bit", 1},
		{"1kbit", 125},
		{"1kbps", 125},
		{"1mbit", 125000},
		{"1mbps", 125000},
		{"1Mbps", 125000}, // case-insensitive
		{"1gbit", 125e6},
		{"512kbps", 64000},
		{"2.5mbit", 312500},
		{" 1mbit ", 125000}, // surrounding space

		// Byte units must say "/s" explicitly.
		{"1B/s", 1},
		{"1kB/s", 1e3},
		{"2MB/s", 2e6},
		{"1GB/s", 1e9},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRate(tc.in)
			if err != nil {
				t.Fatalf("ParseRate(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRate(%q) = %g B/s, want %g", tc.in, got, tc.want)
			}
		})
	}
}

// "1MB/s" is eight times "1mbit". Guessing between them from letter case would
// be a silent factor-of-eight error, so an unsuffixed number must be refused.
func TestParseRate_Rejects(t *testing.T) {
	cases := []struct {
		in      string
		wantSub string
	}{
		{"", "empty"},
		{"1000000", "no unit"},       // ambiguous: bits or bytes?
		{"1", "no unit"},             //
		{"fast", "no unit"},          //
		{"mbit", "missing a number"}, // unit with no number
		{"0mbit", "must be positive"},
		{"-5mbit", "must be positive"},
		{"abcmbit", "not a number"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseRate(tc.in)
			if err == nil {
				t.Fatalf("ParseRate(%q) should have failed", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error should mention %q; got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// The first packet on an idle link goes out immediately; the next waits for
// the first to finish transmitting.
func TestShaper_PacesSerially(t *testing.T) {
	// 1000 B/s → a 100-byte packet occupies the link for 100ms.
	sh := newShaper(1000, time.Second)
	now := time.Unix(0, 0)

	wait, ok := sh.admit(100, now)
	if !ok || wait != 0 {
		t.Fatalf("first packet on an idle link: wait=%v ok=%v, want 0/true", wait, ok)
	}
	wait, ok = sh.admit(100, now)
	if !ok || wait != 100*time.Millisecond {
		t.Fatalf("second packet: wait=%v ok=%v, want 100ms/true", wait, ok)
	}
	wait, ok = sh.admit(100, now)
	if !ok || wait != 200*time.Millisecond {
		t.Fatalf("third packet: wait=%v ok=%v, want 200ms/true", wait, ok)
	}
}

// An idle link must not bank credit indefinitely — the backlog resets to zero
// rather than going negative and letting a burst through for free.
func TestShaper_IdleLinkDoesNotBankCredit(t *testing.T) {
	sh := newShaper(1000, time.Second)
	now := time.Unix(0, 0)

	if _, ok := sh.admit(100, now); !ok {
		t.Fatal("first admit failed")
	}
	// Long after the link drained.
	later := now.Add(10 * time.Second)
	wait, ok := sh.admit(100, later)
	if !ok || wait != 0 {
		t.Errorf("after a long idle: wait=%v ok=%v, want 0/true", wait, ok)
	}
	// And the one after it queues normally, not from a banked surplus.
	wait, ok = sh.admit(100, later)
	if !ok || wait != 100*time.Millisecond {
		t.Errorf("next packet: wait=%v ok=%v, want 100ms/true", wait, ok)
	}
}

// A rate limiter with an unbounded queue is a memory leak with latency: the
// sender never observes congestion. Real bottlenecks drop, and the drop is
// what makes a congestion-control bug reproducible.
func TestShaper_DropsWhenBacklogExceeded(t *testing.T) {
	sh := newShaper(1000, 250*time.Millisecond) // 250ms of queue
	now := time.Unix(0, 0)

	// Each 100-byte packet buys 100ms of backlog.
	for i := 0; i < 3; i++ {
		if _, ok := sh.admit(100, now); !ok {
			t.Fatalf("packet %d should have been admitted", i)
		}
	}
	// Backlog is now 300ms > 250ms, so the next one is dropped.
	if _, ok := sh.admit(100, now); ok {
		t.Error("packet admitted with the queue over its limit; the shaper must drop")
	}
	if got := sh.stats().Dropped; got != 1 {
		t.Errorf("Dropped = %d, want 1", got)
	}
	if got := sh.stats().Admitted; got != 3 {
		t.Errorf("Admitted = %d, want 3", got)
	}

	// Once the link drains, it accepts again — a full queue is transient.
	if _, ok := sh.admit(100, now.Add(time.Second)); !ok {
		t.Error("shaper must recover once the backlog drains")
	}
}

func TestShaper_RecordsPeakBacklog(t *testing.T) {
	sh := newShaper(1000, time.Second)
	now := time.Unix(0, 0)
	for i := 0; i < 4; i++ {
		sh.admit(100, now)
	}
	// Waits were 0, 100ms, 200ms, 300ms — peak observed is 300ms.
	if got := sh.stats().PeakBacklog; got != 300*time.Millisecond {
		t.Errorf("PeakBacklog = %v, want 300ms", got)
	}
}

func TestShaper_ZeroSizeIsFree(t *testing.T) {
	sh := newShaper(1000, time.Second)
	wait, ok := sh.admit(0, time.Unix(0, 0))
	if !ok || wait != 0 {
		t.Errorf("a zero-length packet must not consume link time: wait=%v ok=%v", wait, ok)
	}
}

func TestDefaultBacklogUsedWhenUnset(t *testing.T) {
	sh := newShaper(1000, 0)
	if sh.maxBacklog != DefaultShaperBacklog {
		t.Errorf("maxBacklog = %v, want the default %v", sh.maxBacklog, DefaultShaperBacklog)
	}
}

// ─── endpoint-level ────────────────────────────────────────────────────────

// mtu() must change what netstack believes the link is, not merely filter
// oversized packets. MTU() is what netstack reads to pick a TCP MSS and an IP
// fragmentation threshold.
func TestEndpointMTUOverride(t *testing.T) {
	fe, _, lower := newTestEndpoint(t, Options{})
	base := lower.MTU()
	if got := fe.MTU(); got != base {
		t.Fatalf("unshaped MTU = %d, want the lower endpoint's %d", got, base)
	}

	fe.SetMTU(576)
	if got := fe.MTU(); got != 576 {
		t.Errorf("MTU after override = %d, want 576", got)
	}

	fe.SetMTU(0)
	if got := fe.MTU(); got != base {
		t.Errorf("MTU after clearing = %d, want the lower endpoint's %d", got, base)
	}
}

// Bandwidth must apply with no rules installed: it describes the link, not a
// packet. The no-rules fast path is exactly where a naive implementation would
// skip it.
func TestBandwidthAppliesWithNoRules(t *testing.T) {
	clock := newFakeClock()
	fe, _, lower := newTestEndpoint(t, Options{Clock: clock})
	// No SetRules call at all.

	// 1000 B/s. defaultPkt is small, so give each packet a known cost by
	// checking relative ordering rather than exact byte math.
	fe.SetBandwidth(DirS2C, 1000, time.Second)

	writeOne(fe, makePacket(defaultPkt()))
	if got := lower.count(); got != 1 {
		t.Fatalf("first packet on an idle link should go straight out, got %d", got)
	}

	writeOne(fe, makePacket(defaultPkt()))
	if got := lower.count(); got != 1 {
		t.Fatalf("second packet should have been paced, but %d were written", got)
	}
	advanceWhenArmed(t, clock, fe.deferQ, 1, time.Second)
	if !waitFor(t, 2*time.Second, func() bool { return lower.count() == 2 }) {
		t.Fatalf("paced packet never released (count=%d)", lower.count())
	}
}

func TestBandwidthDirectionIsRespected(t *testing.T) {
	clock := newFakeClock()
	fe, disp, _ := newTestEndpoint(t, Options{Clock: clock})
	// Shape only egress; ingress must stay at full speed.
	fe.SetBandwidth(DirS2C, 1000, time.Second)

	for i := 0; i < 5; i++ {
		fe.DeliverNetworkPacket(header.IPv4ProtocolNumber, makePacket(defaultPkt()))
	}
	if got := disp.count(); got != 5 {
		t.Errorf("ingress delivered %d of 5; an s2c shaper must not pace c2s", got)
	}
}

func TestClearShapersRestoresTheLink(t *testing.T) {
	fe, _, lower := newTestEndpoint(t, Options{})
	base := lower.MTU()

	fe.SetBandwidth(DirBoth, 1000, time.Second)
	fe.SetMTU(576)
	if _, ok := fe.ShaperStatsFor(DirS2C); !ok {
		t.Fatal("expected an s2c shaper to be installed")
	}

	fe.ClearShapers()
	if _, ok := fe.ShaperStatsFor(DirS2C); ok {
		t.Error("s2c shaper survived ClearShapers")
	}
	if _, ok := fe.ShaperStatsFor(DirC2S); ok {
		t.Error("c2s shaper survived ClearShapers")
	}
	if got := fe.MTU(); got != base {
		t.Errorf("MTU = %d after ClearShapers, want %d", got, base)
	}
}
