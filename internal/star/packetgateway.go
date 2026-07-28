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
	// InstallRules replaces the packet rules on one service interface.
	InstallRules(service, iface string, rules []*netfault.Rule) error
	// ClearRules removes them.
	ClearRules(service, iface string) error
}

// packetRuleRegistry records what the spec asked for, per interface. It exists
// so a fault window can install and then reliably clear, and so the runtime can
// report rules that matched nothing.
type packetRuleRegistry struct {
	mu       sync.Mutex
	gateway  PacketGateway
	unwired  int
	installs map[string][]*netfault.Rule
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

func regKey(service, iface string) string { return service + "\x00" + iface }

// install pushes rules to the gateway, or counts the attempt when no gateway
// is attached.
func (r *packetRuleRegistry) install(service, iface string, rules []*netfault.Rule) error {
	r.mu.Lock()
	g := r.gateway
	r.installs[regKey(service, iface)] = rules
	if g == nil {
		r.unwired++
	}
	r.mu.Unlock()
	if g == nil {
		return nil
	}
	return g.InstallRules(service, iface, rules)
}

func (r *packetRuleRegistry) clear(service, iface string) error {
	r.mu.Lock()
	g := r.gateway
	delete(r.installs, regKey(service, iface))
	r.mu.Unlock()
	if g == nil {
		return nil
	}
	return g.ClearRules(service, iface)
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
func (r *packetRuleRegistry) rulesFor(service, iface string) []*netfault.Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installs[regKey(service, iface)]
}

// applyPacketFaults compiles and installs a set of packet fault defs for the
// duration of a fault window. Returns a cleanup func.
func (rt *Runtime) applyPacketFaults(thread *starlark.Thread, svcName, ifaceName string, defs []*PacketFaultDef) (func(), error) {
	rules := make([]*netfault.Rule, 0, len(defs))
	for _, d := range defs {
		r, err := d.Compile(thread)
		if err != nil {
			return nil, fmt.Errorf("packet_%s(): %w", d.Action, err)
		}
		rules = append(rules, r)
	}
	if err := rt.packetRules.install(svcName, ifaceName, rules); err != nil {
		return nil, fmt.Errorf("install packet rules on %s.%s: %w", svcName, ifaceName, err)
	}

	rt.events.Emit("packet_fault_applied", svcName, map[string]string{
		"interface": ifaceName,
		"rules":     describePacketFaults(defs),
		"count":     fmt.Sprintf("%d", len(rules)),
	})

	return func() {
		_ = rt.packetRules.clear(svcName, ifaceName)
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
