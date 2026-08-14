package star

import (
	"fmt"
	"strings"
)

// packetFaultCallNames are the spec-level calls that need a netstack
// gateway. Used only to decide whether to preflight it — a false
// positive costs one attach attempt, a false negative costs a whole run.
var packetFaultCallNames = []string{
	"partition(",
	"partition_start(",
	"packet_drop(",
	"packet_delay(",
	"packet_reorder(",
	"packet_duplicate(",
	"packet_corrupt(",
	"packet_reset(",
	"packet_window(",
	"packet_pass(",
	"bandwidth(",
	"mtu(",
}

// specUsesPacketFaults reports whether the spec calls anything that needs
// the packet gateway. Statement-folded and whitespace-stripped, so a
// multi-line call is seen — see specStatements.
func (rt *Runtime) specUsesPacketFaults() bool {
	src := strings.Join(specStatements(rt.sourceText), "\n")
	for _, b := range packetFaultCallNames {
		if strings.Contains(src, b) {
			return true
		}
	}
	return false
}

// preflightPacketGateway attaches the netstack gateway before the test
// body runs, when the spec is going to need it.
//
// Without this, a gateway that cannot attach is only noticed after the
// body has finished, by counting packet rules that were installed
// against nothing:
//
//	packet faults were installed 2 time(s) but no netstack gateway was
//	attached, so no packet was affected
//
// That check is right to refuse the result, but it arrives late and does
// not say *why* the gateway is missing — the actual cause (no
// CAP_NET_ADMIN, no /dev/net/tun, a leftover TUN device) is logged at
// attach time and easy to miss. Failing here instead reports the cause
// as a setup error, before the run spends its time (F-7).
//
// A spec that declares packet faults but never reaches them on a given
// leaf still fails here. That is deliberate: the gateway is topology, not
// a per-test decision, and a run that cannot mediate packets cannot be
// trusted to report on packet-level behaviour whichever branch it takes.
func (rt *Runtime) preflightPacketGateway() error {
	if !rt.packetGatewayEnabled() || !rt.specUsesPacketFaults() {
		return nil
	}
	if _, err := rt.ensurePacketGateway(); err != nil {
		return fmt.Errorf("the netstack gateway could not attach: %w", err)
	}
	return nil
}

// notePacketGatewayPreflight attempts the attach and records why it
// failed, without failing the test.
//
// The reporter asked for a setup-time failure instead of the post-hoc
// one, and that cannot be done as stated: packet faults are body-time
// calls, so their arguments are validated inside the body. Failing at
// setup pre-empts that, replacing a spec error the author can fix with an
// environment error they cannot — verified against three existing
// validation tests, which failed exactly that way on both platforms.
//
// What was actually missing is the *reason*. The post-hoc check said "no
// netstack gateway was attached" and stopped; the cause — no
// CAP_NET_ADMIN, no /dev/net/tun, a leftover device from a killed run —
// was logged at attach time and easy to miss. Recording it here lets the
// failure carry it.
func (rt *Runtime) notePacketGatewayPreflight() {
	err := rt.preflightPacketGateway()
	rt.packetRules.mu.Lock()
	rt.packetRules.attachErr = err
	rt.packetRules.mu.Unlock()
}

// packetGatewayAttachReason returns the recorded attach failure, or "".
func (rt *Runtime) packetGatewayAttachReason() string {
	rt.packetRules.mu.Lock()
	defer rt.packetRules.mu.Unlock()
	if rt.packetRules.attachErr == nil {
		return ""
	}
	return rt.packetRules.attachErr.Error()
}
