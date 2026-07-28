package star

import (
	"fmt"
	"sync"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// Gateway lifecycle and address resolution (RFC-054 M4).
//
// The gateway chains *behind* the existing protocol proxies rather than
// replacing them:
//
//	SUT → [TUN → netstack → packet rules] → gateway relay → proxy → upstream
//
// So the SUT dials a gateway address, packet faults act on the leg the SUT
// actually experiences, and the 14 protocol proxies keep their listeners
// unchanged.

// packetGatewayState holds the single spec-wide gateway.
type packetGatewayState struct {
	mu      sync.Mutex
	gw      *netfault.Gateway
	started bool
	// served tracks which addresses already have a relay running, so
	// resolving the same triple twice does not start a second one.
	served map[string]bool
}

func newPacketGatewayState() *packetGatewayState {
	return &packetGatewayState{served: make(map[string]bool)}
}

// packetGatewayEnabled reports whether the spec's runtime provides one.
func (rt *Runtime) packetGatewayEnabled() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return runtimeSupportsPacketFaults(rt.detRuntime)
}

// ensurePacketGateway creates and starts the gateway on first use.
//
// Preflight runs before anything else so a missing capability is reported as
// a setup error naming the fix, rather than as a connection timeout several
// seconds into the first test.
func (rt *Runtime) ensurePacketGateway() (*netfault.Gateway, error) {
	if !rt.packetGatewayEnabled() {
		return nil, nil
	}
	st := rt.packetGW
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.started {
		return st.gw, nil
	}

	gw, err := netfault.NewGateway(netfault.GatewayConfig{
		Seed:    rt.gatewaySeed(),
		OnEvent: rt.emitPacketEvent,
	})
	if err != nil {
		return nil, fmt.Errorf("packet gateway: %w", err)
	}
	if err := gw.Preflight(); err != nil {
		return nil, fmt.Errorf("packet gateway preflight: %w", err)
	}
	if err := gw.Start(); err != nil {
		return nil, fmt.Errorf("start packet gateway: %w", err)
	}

	st.gw = gw
	st.started = true
	// Rules installed by fault() now reach real traffic, which is what stops
	// RunTest failing the test for an unwired gateway.
	rt.packetRules.SetGateway(gw)

	rt.events.Emit("packet_gateway_started", "", map[string]string{
		"device": gw.Device(),
		"subnet": gw.Subnet(),
		"host":   gw.HostIP(),
	})
	return gw, nil
}

// gatewaySeed returns the spec seed for probabilistic packet rules, or 0 when
// the spec did not set one — matching how the rest of the runtime treats an
// unset seed.
func (rt *Runtime) gatewaySeed() int64 {
	if rt.seed == nil {
		return 0
	}
	return int64(*rt.seed)
}

// gatewayAddrFor returns the address `consumer` should dial to reach
// service.iface through the packet gateway, starting the relay on first use.
// Returns "" when no gateway is active, so callers fall back to the proxy.
func (rt *Runtime) gatewayAddrFor(consumer, service, iface string, port int, upstream string) string {
	if !rt.packetGatewayEnabled() || upstream == "" {
		return ""
	}
	gw, err := rt.ensurePacketGateway()
	if err != nil || gw == nil {
		if err != nil {
			rt.log.Error("packet gateway unavailable", "error", err.Error())
		}
		return ""
	}

	ga, err := gw.Allocate(consumer, service, iface, port, upstream)
	if err != nil {
		rt.log.Error("packet gateway address allocation failed", "error", err.Error())
		return ""
	}

	st := rt.packetGW
	st.mu.Lock()
	already := st.served[ga.Addr()]
	if !already {
		st.served[ga.Addr()] = true
	}
	st.mu.Unlock()

	if !already {
		if err := gw.Serve(ga); err != nil {
			rt.log.Error("packet gateway listen failed",
				"addr", ga.Addr(), "error", err.Error())
			st.mu.Lock()
			delete(st.served, ga.Addr())
			st.mu.Unlock()
			return ""
		}
		rt.events.Emit("packet_gateway_route", service, map[string]string{
			"interface": iface,
			"consumer":  consumer,
			"addr":      ga.Addr(),
			"upstream":  upstream,
		})
	}
	return ga.Addr()
}

// closePacketGateway tears the gateway down at session end.
func (rt *Runtime) closePacketGateway() {
	st := rt.packetGW
	st.mu.Lock()
	gw := st.gw
	st.gw, st.started, st.served = nil, false, make(map[string]bool)
	st.mu.Unlock()
	if gw == nil {
		return
	}

	// Report anything the datapath had to skip. A corrupt or window rule that
	// matched but could not be applied injected nothing, and a run that says
	// nothing about it looks like a clean pass.
	if ep := gw.Endpoint(); ep != nil {
		if n := ep.MutationsSkipped(); n > 0 {
			rt.events.Emit("packet_mutation_skipped", "", map[string]string{
				"count":  fmt.Sprintf("%d", n),
				"detail": "packet spanned multiple buffer views; corrupt/window could not be applied",
			})
		}
	}
	if err := gw.Close(); err != nil {
		rt.log.Warn("packet gateway close", "error", err.Error())
	}
	rt.packetRules.SetGateway(nil)
	rt.events.Emit("packet_gateway_stopped", "", nil)
}

// emitPacketEvent turns a netfault event into a `packet` event-log entry.
// Field names mirror the `proxy` family, including the action/protocol fields
// added in v0.13.3.
func (rt *Runtime) emitPacketEvent(e netfault.Event) {
	fields := map[string]string{
		"action":    e.Action.String(),
		"protocol":  string(e.Protocol),
		"direction": e.Direction.String(),
		"src":       e.Src,
		"dst":       e.Dst,
		"len":       fmt.Sprintf("%d", e.Len),
		"flow":      e.Flow,
	}
	if len(e.Flags) > 0 {
		fields["flags"] = joinComma(e.Flags)
	}
	if e.RuleLabel != "" {
		fields["label"] = e.RuleLabel
	}
	rt.events.Emit("packet", "", fields)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
