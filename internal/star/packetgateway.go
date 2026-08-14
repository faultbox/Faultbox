package star

import (
	"fmt"
	"sync"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// PacketGateway is what the netstack data path must provide for packet faults
// to actually reach traffic. The Linux TUN implementation lands in M4; this
// interface is the seam so the DSL and the gateway can be built and tested
// independently.
type PacketGateway interface {
	// InstallRules replaces the packet rules on one (consumer, service,
	// interface) triple. An empty consumer means every consumer.
	InstallRules(consumer, service, iface string, rules []*netfault.Rule) error
	// ClearRules removes them.
	ClearRules(consumer, service, iface string) error
}

// packetRuleRegistry records what the spec asked for, per interface. It exists
// so a fault window can install and then reliably clear, and so the runtime can
// report rules that matched nothing.
type packetRuleRegistry struct {
	mu       sync.Mutex
	gateway  PacketGateway
	unwired  int
	installs map[string][]*netfault.Rule

	// whereErr is the first where=-predicate failure seen this test, and
	// whereErrCount how many followed. A lambda that throws on every packet
	// matches nothing, so the fault silently never fires and the test passes.
	whereErr      error
	whereErrCount int

	// attachErr is why the netstack gateway could not attach, recorded at
	// setup so the "no gateway was attached" failure can say what the
	// cause was instead of only that it happened.
	attachErr error
}

// recordWhereError captures a failing where= predicate. Called from the
// datapath, so it must be cheap and must not block.
func (rt *Runtime) recordWhereError(err error) {
	r := rt.packetRules
	r.mu.Lock()
	defer r.mu.Unlock()
	r.whereErrCount++
	if r.whereErr == nil {
		r.whereErr = err
	}
}

// firstWhereError returns the first where= failure and the total count.
func (r *packetRuleRegistry) firstWhereError() (error, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.whereErr, r.whereErrCount
}

func newPacketRuleRegistry() *packetRuleRegistry {
	return &packetRuleRegistry{installs: make(map[string][]*netfault.Rule)}
}

// SetGateway attaches the data path. Called by the service-start path in M4.
func (r *packetRuleRegistry) SetGateway(g PacketGateway) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gateway = g
}

func regKey(consumer, service, iface string) string {
	return consumer + "\x00" + service + "\x00" + iface
}

// install pushes rules to the gateway, or counts the attempt when no gateway
// is attached.
func (r *packetRuleRegistry) install(consumer, service, iface string, rules []*netfault.Rule) error {
	r.mu.Lock()
	g := r.gateway
	r.installs[regKey(consumer, service, iface)] = rules
	if g == nil {
		r.unwired++
	}
	r.mu.Unlock()
	if g == nil {
		return nil
	}
	return g.InstallRules(consumer, service, iface, rules)
}

func (r *packetRuleRegistry) clear(consumer, service, iface string) error {
	r.mu.Lock()
	g := r.gateway
	delete(r.installs, regKey(consumer, service, iface))
	r.mu.Unlock()
	if g == nil {
		return nil
	}
	return g.ClearRules(consumer, service, iface)
}

// unwiredInstalls reports how many packet-fault windows ran with no data path
// behind them.
//
// This number must never be silently discarded. A packet fault that installs
// into nothing produces a *passing* test, and the author concludes their
// service tolerates packet loss when no packet was ever touched. Until the M4
// gateway lands, every packet fault is in exactly that state, so the runtime
// surfaces it as a test-failing condition rather than a log line.
func (r *packetRuleRegistry) unwiredInstalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unwired
}

// rulesFor returns the rules currently installed on an interface. Test-facing.
func (r *packetRuleRegistry) rulesFor(consumer, service, iface string) []*netfault.Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installs[regKey(consumer, service, iface)]
}

// applyPacketFaults compiles and installs a set of packet fault defs for the
// duration of a fault window. Returns a cleanup func.
func (rt *Runtime) applyPacketFaults(thread *starlark.Thread, consumer, svcName, ifaceName string, defs []*PacketFaultDef) (func(), error) {
	rules := make([]*netfault.Rule, 0, len(defs))
	for _, d := range defs {
		r, err := d.Compile(thread, rt.recordWhereError)
		if err != nil {
			return nil, fmt.Errorf("packet_%s(): %w", d.Action, err)
		}
		rules = append(rules, r)
	}
	if err := rt.packetRules.install(consumer, svcName, ifaceName, rules); err != nil {
		return nil, fmt.Errorf("install packet rules on %s.%s: %w", svcName, ifaceName, err)
	}

	fields := map[string]string{
		"interface": ifaceName,
		"rules":     describePacketFaults(defs),
		"count":     fmt.Sprintf("%d", len(rules)),
	}
	if consumer != "" {
		fields["source"] = consumer
	}
	rt.events.Emit("packet_fault_applied", svcName, fields)

	return func() {
		_ = rt.packetRules.clear(consumer, svcName, ifaceName)
		for i, r := range rules {
			if r.MatchCount() == 0 {
				rt.events.Emit("packet_fault_no_match", svcName, map[string]string{
					"interface": ifaceName,
					"rule":      fmt.Sprintf("packet_%s", defs[i].Action),
					"label":     defs[i].Label,
				})
			}
		}
	}, nil
}
