package netfault

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// GatewayConfig configures the packet gateway.
type GatewayConfig struct {
	// Device is the TUN interface name. Defaults to "faultbox0".
	Device string
	// Subnet is the address space the gateway owns. Defaults to DefaultSubnet.
	Subnet string
	// Seed drives probabilistic packet rules.
	Seed int64
	// OnEvent receives packet fault events.
	OnEvent OnEvent
	// Decider is the RFC-042 §8.9 plan-leaf oracle.
	Decider ProbabilityDecider
}

func (c *GatewayConfig) withDefaults() GatewayConfig {
	out := *c
	if out.Device == "" {
		out.Device = "faultbox0"
	}
	if out.Subnet == "" {
		out.Subnet = DefaultSubnet
	}
	return out
}

// Gateway is the packet-level data path: the SUT dials an address the gateway
// owns, netstack terminates the connection, packet rules act on every segment
// in both directions, and the gateway relays the payload to the real upstream.
//
// RFC-054 decision record M0.2 chose this shape (TUN + routed subnet) over
// AF_PACKET on a veth peer, because AF_PACKET copies frames the kernel has
// already delivered — it can observe but cannot drop.
type Gateway struct {
	cfg   GatewayConfig
	alloc *addrAllocator

	mu        sync.Mutex
	rules     map[string]*RuleSet // "service\x00iface" -> rules
	listeners []net.Listener
	closed    bool

	// endpoint is the FaultEndpoint spliced under the netstack NIC. nil until
	// Start, and always nil on platforms without a TUN implementation.
	endpoint *FaultEndpoint

	platform platformGateway
}

// platformGateway is the OS-specific half: create the TUN, run netstack over
// it, and hand out listeners. Linux implements it; everything else returns a
// clear "not supported" error, so the cross-platform half stays testable.
type platformGateway interface {
	start(g *Gateway) error
	listen(g *Gateway, ga *GatewayAddr) (net.Listener, error)
	// close stops netstack and releases the TUN fd.
	close() error
	// destroy removes the TUN device, but only if this gateway created it.
	// Separate from close because the device outlives the netstack instance:
	// a leaked one survives the process and breaks the next run with
	// "device busy".
	destroy(g *Gateway)
	// preflight reports why the gateway cannot run here, or nil.
	preflight(g *Gateway) error
}

// NewGateway builds a gateway. It does not touch the network until Start.
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	c := cfg.withDefaults()
	alloc, err := newAddrAllocator(c.Subnet)
	if err != nil {
		return nil, err
	}
	return &Gateway{
		cfg:      c,
		alloc:    alloc,
		rules:    make(map[string]*RuleSet),
		platform: newPlatformGateway(),
	}, nil
}

// Preflight reports whether this host can run the gateway, with an actionable
// message naming the remediation when it cannot.
//
// Checked before the first service starts so the failure is a clear setup
// error rather than a mid-test connection timeout.
func (g *Gateway) Preflight() error { return g.platform.preflight(g) }

// HostIP is the address assigned to the host end of the TUN device.
func (g *Gateway) HostIP() string { return g.alloc.hostIP() }

// Subnet returns the CIDR the gateway owns.
func (g *Gateway) Subnet() string { return g.cfg.Subnet }

// Device returns the TUN interface name.
func (g *Gateway) Device() string { return g.cfg.Device }

// Allocate reserves an address for a (consumer, service, interface) triple and
// returns the address the SUT should dial instead of the real upstream.
func (g *Gateway) Allocate(consumer, service, iface string, port int, upstream string) (*GatewayAddr, error) {
	return g.alloc.allocate(consumer, service, iface, port, upstream)
}

// Lookup returns a previously allocated address.
func (g *Gateway) Lookup(consumer, service, iface string) (*GatewayAddr, bool) {
	return g.alloc.lookup(consumer, service, iface)
}

// Start brings up the TUN device and the netstack instance.
func (g *Gateway) Start() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return fmt.Errorf("gateway is closed")
	}
	g.mu.Unlock()

	if err := g.platform.preflight(g); err != nil {
		return err
	}
	return g.platform.start(g)
}

// Listen returns a listener on a gateway address. Connections arriving on it
// have already crossed the packet fault rules.
func (g *Gateway) Listen(ga *GatewayAddr) (net.Listener, error) {
	ln, err := g.platform.listen(g, ga)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.listeners = append(g.listeners, ln)
	g.mu.Unlock()
	return ln, nil
}

// Serve accepts on a gateway address and relays each connection to the
// address's upstream, which is normally the existing protocol proxy.
//
// Chaining behind the proxy rather than replacing it is deliberate. The SUT
// dials the gateway, so packet faults act on the SUT-facing leg — the one the
// SUT actually experiences — while the 14 protocol proxies keep their existing
// listeners and need no changes at all. The cost is one extra loopback hop,
// the same order as the proxy hop already present since RFC-024.
//
// Serve returns once the listener is closed.
func (g *Gateway) Serve(ga *GatewayAddr) error {
	ln, err := g.Listen(ga)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed during teardown
			}
			go g.relay(c, ga.Upstream)
		}
	}()
	return nil
}

func (g *Gateway) relay(down net.Conn, upstream string) {
	defer down.Close()
	if upstream == "" {
		return
	}
	up, err := net.DialTimeout("tcp", upstream, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(up, down); closeWrite(up) }()
	go func() { defer wg.Done(); io.Copy(down, up); closeWrite(down) }()
	wg.Wait()
}

// closeWrite half-closes so the peer observes EOF rather than waiting for the
// whole connection to drop. Without it a request/response protocol that reads
// until EOF hangs for the full timeout.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func ruleKey(service, iface string) string { return service + "\x00" + iface }

// InstallRules implements the star.PacketGateway seam.
func (g *Gateway) InstallRules(service, iface string, rules []*Rule) error {
	rs, err := NewRuleSet(rules...)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.rules[ruleKey(service, iface)] = rs
	g.mu.Unlock()
	g.republish()
	return nil
}

// ClearRules implements the star.PacketGateway seam.
func (g *Gateway) ClearRules(service, iface string) error {
	g.mu.Lock()
	delete(g.rules, ruleKey(service, iface))
	g.mu.Unlock()
	g.republish()
	return nil
}

// republish flattens the per-interface rule sets onto the single endpoint.
//
// There is one netstack NIC and therefore one FaultEndpoint, so rules from
// every faulted interface share a rule list. Scoping is by destination
// address, which the allocator guarantees is unique per interface — so a rule
// installed for db.main gets a Port predicate that cannot fire on cache.main.
func (g *Gateway) republish() {
	g.mu.Lock()
	var all []*Rule
	keys := make([]string, 0, len(g.rules))
	for k := range g.rules {
		keys = append(keys, k)
	}
	// Deterministic order: rule precedence must not depend on map iteration.
	sortStrings(keys)
	for _, k := range keys {
		if rs := g.rules[k]; rs != nil {
			all = append(all, rs.Rules()...)
		}
	}
	ep := g.endpoint
	g.mu.Unlock()

	if ep == nil {
		return
	}
	if len(all) == 0 {
		ep.SetRules(nil)
		return
	}
	rs, err := NewRuleSet(all...)
	if err != nil {
		return // already validated at install time
	}
	ep.SetRules(rs)
}

// Endpoint exposes the FaultEndpoint for reporting (where= evaluation counts,
// skipped mutations). nil before Start.
func (g *Gateway) Endpoint() *FaultEndpoint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.endpoint
}

// Close tears down listeners, the netstack instance, and the TUN device.
//
// Teardown is explicitly tested: a leaked TUN device survives the process and
// breaks the next run with "device busy", and the proxy-lifecycle findings
// (docs/design/2026-04-27) are a standing reminder that this layer leaks
// quietly when nobody asserts on it.
func (g *Gateway) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	lns := g.listeners
	g.listeners = nil
	g.mu.Unlock()

	var firstErr error
	for _, ln := range lns {
		if err := ln.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := g.platform.close(); err != nil && firstErr == nil {
		firstErr = err
	}
	// Remove the TUN device too. Leaving it behind is a silent leak that only
	// shows up as "device busy" on the *next* run, which is a miserable way to
	// discover a teardown bug.
	g.platform.destroy(g)
	return firstErr
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
