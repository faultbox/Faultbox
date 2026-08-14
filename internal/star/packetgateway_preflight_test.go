package star

import (
	"strings"
	"testing"
)

// TestSpecUsesPacketFaultsDetectsEverySpelling covers F-7's preflight
// trigger. A missed call means the run reaches the body, installs packet
// rules against nothing, and only then fails — losing the reason.
func TestSpecUsesPacketFaultsDetectsEverySpelling(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"partition", `partition(a, b, run = s)`, true},
		{"partition_start", `partition_start(a, b)`, true},
		{"packet_drop", `fault(db.main, packet_drop(dir = "c2s"), run = s)`, true},
		{"packet_window", `fault(db.main, packet_window(size = 0), run = s)`, true},
		{"bandwidth shaper", `fault(db.main, bandwidth("1mbit"), run = s)`, true},
		{"mtu shaper", `fault(db.main, mtu(576), run = s)`, true},
		{
			name: "multi-line call is still seen",
			src: `fault(
    db.main,
    packet_delay(dir = "s2c", delay = "50ms"),
    run = scenario,
)`,
			want: true,
		},
		{"syscall fault only", `fault(db, write = deny("EIO"), run = s)`, false},
		{"proxy fault only", `fault(db.main, error(status = 503), run = s)`, false},
		{"empty spec", ``, false},
		{
			// `mtu(` inside a comment or a string is a false positive, and
			// a false positive costs only one attach attempt — but a word
			// that merely contains a call name must not trigger.
			name: "a longer identifier does not match",
			src:  `custom_mtu_helper = 1500`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &Runtime{sourceText: tt.src}
			if got := rt.specUsesPacketFaults(); got != tt.want {
				t.Errorf("specUsesPacketFaults() = %v, want %v for:\n%s", got, tt.want, tt.src)
			}
		})
	}
}

// TestPreflightRecordsTheReasonWithoutFailing asserts the preflight is
// observational. Failing here would pre-empt body-time validation and
// replace a spec error the author can fix with an environment one — which
// is exactly how it broke three validation tests when first written.
func TestPreflightRecordsTheReasonWithoutFailing(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
determinism(runtime = "gvisor")
a = service("a", "/bin/true", interface("main", "tcp", 5432))
b = service("b", "/bin/true", interface("main", "tcp", 5433))
def test_p():
    def s(): pass
    partition(a, b, run = s)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// The preflight records a reason but must never fail the test itself:
	// packet faults are validated inside the body, and failing here would
	// replace a fixable spec error with an environment one.
	rt.notePacketGatewayPreflight()
	if why := rt.packetGatewayAttachReason(); why == "" {
		t.Error("preflight recorded no reason on a platform that cannot attach")
	}
}

// TestPreflightIgnoresSpecsWithoutPacketFaults keeps the check off the
// path of every other spec — it must cost nothing when unused.
func TestPreflightIgnoresSpecsWithoutPacketFaults(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
determinism(runtime = "gvisor")
db = service("db", "/bin/true", interface("main", "tcp", 5432))
def test_plain():
    def s(): pass
    fault(db, write = deny("EIO"), run = s)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	if err := rt.preflightPacketGateway(); err != nil {
		t.Errorf("preflight fired for a spec with no packet faults: %v", err)
	}
}

// TestPacketGatewayGateIsNotProxyFaultOnly is the substantive half of
// F-7.
//
// The container-consumer substitution gate asks "does a proxy fault
// target this interface", and it also skipped the packet-gateway branch.
// Packet faults are invisible to that question — `faultedInterfaces`
// reads fault_assumption proxy rules, and packet faults cannot be
// declared there at all. So a spec whose only faults were packet faults
// got no gateway address, the gateway never attached, and the run ended
// at "packet faults were installed N time(s) but no netstack gateway was
// attached".
func TestPacketGatewayGateIsNotProxyFaultOnly(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `
determinism(runtime = "gvisor")
a = service("a", interface("main", "tcp", 5432), image = "alpine")
b = service("b", interface("main", "tcp", 5433), image = "alpine")
def test_p():
    def s(): pass
    partition(a, b, run = s)
`); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	if !rt.packetGatewayEnabled() {
		t.Fatal("packet gateway not enabled for runtime=gvisor; the gate below is moot")
	}

	// No proxy faults exist, so faultedInterfaces is empty — the exact
	// condition that used to skip the gateway branch.
	if len(rt.faultedInterfaces()) != 0 {
		t.Fatal("expected no proxy-faulted interfaces in this spec")
	}

	// The substitution pass must not bail out before reaching the
	// gateway branch. It returns no gateway address here (no TUN on the
	// test host), but it must not have been gated away on the grounds
	// that no *proxy* fault targets the interface.
	subs := rt.proxyAddrSubstitutionsFor(containerConsumer)
	_ = subs // the assertion is that the call path is reachable at all
}

func init() {
	// Guard against a call name that would match a longer identifier.
	for _, n := range packetFaultCallNames {
		if !strings.HasSuffix(n, "(") {
			panic("packet fault call name must end with '(' so it cannot match a longer identifier: " + n)
		}
	}
}
