package netfault

import (
	"fmt"
	"net"
	"sort"
	"sync"
)

// DefaultSubnet is the address space the gateway owns. Chosen to be unlikely
// to collide with a Docker bridge (172.17/16), a typical corporate 10.0/8
// allocation, or the 192.168/16 home range.
const DefaultSubnet = "10.99.0.0/16"

// GatewayAddr is one netstack address, dedicated to a single
// (consumer, target service, target interface) triple.
//
// One address per triple — rather than one shared address with per-flow
// bookkeeping — is what makes `fault(db.main, source=worker)` work without the
// packet's source IP. Docker masquerades container traffic, so the source IP
// arrives as the bridge gateway and cannot identify the sender; the
// destination can, because we chose it. See RFC-054 decision record M0.2.
type GatewayAddr struct {
	// Consumer is the service dialing this address, or "" for "any consumer".
	Consumer string
	// Service and Interface identify what is being dialed.
	Service   string
	Interface string
	// IP is the netstack address, e.g. "10.99.0.5".
	IP string
	// Port mirrors the target interface's port so the SUT's connection string
	// changes only in host, not in port. Fewer moving parts when a spec
	// author reads FAULTBOX_*_ADDR and compares it to the real upstream.
	Port int
	// Upstream is the real address to relay to, "host:port".
	Upstream string
}

// Addr renders the dialable "ip:port".
func (a GatewayAddr) Addr() string { return fmt.Sprintf("%s:%d", a.IP, a.Port) }

// key identifies the triple this address serves.
func (a GatewayAddr) key() string {
	return a.Consumer + "\x00" + a.Service + "\x00" + a.Interface
}

// addrAllocator hands out addresses from the gateway subnet.
//
// Allocation is deterministic: the same set of triples, requested in any
// order, yields the same assignment. That matters because the address ends up
// in the SUT's environment and therefore in the event log and the .fb bundle,
// and a bundle that replays with different addresses is confusing at best.
type addrAllocator struct {
	mu     sync.Mutex
	base   net.IP
	ones   int
	bits   int
	byKey  map[string]*GatewayAddr
	nextID int
}

func newAddrAllocator(cidr string) (*addrAllocator, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse gateway subnet %q: %w", cidr, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, fmt.Errorf("gateway subnet %q must be IPv4", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 8 {
		return nil, fmt.Errorf("gateway subnet %q is too small; need at least a /24", cidr)
	}
	return &addrAllocator{
		base:  v4.Mask(ipnet.Mask),
		ones:  ones,
		bits:  bits,
		byKey: make(map[string]*GatewayAddr),
		// .1 is the host side of the TUN; start guests at .2.
		nextID: 2,
	}, nil
}

// hostIP is the address assigned to the host end of the TUN device.
func (a *addrAllocator) hostIP() string {
	return a.ipForOffset(1)
}

func (a *addrAllocator) ipForOffset(n int) string {
	v := make(net.IP, 4)
	copy(v, a.base)
	// Little-endian fill across the host portion.
	v[3] = byte(n & 0xFF)
	v[2] = byte((n >> 8) & 0xFF)
	return v.String()
}

// allocate returns the address for a triple, creating it on first request.
func (a *addrAllocator) allocate(consumer, service, iface string, port int, upstream string) (*GatewayAddr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	probe := GatewayAddr{Consumer: consumer, Service: service, Interface: iface}
	if existing, ok := a.byKey[probe.key()]; ok {
		return existing, nil
	}

	maxHosts := 1<<(a.bits-a.ones) - 2
	if a.nextID > maxHosts {
		return nil, fmt.Errorf("gateway subnet exhausted after %d addresses; "+
			"widen the subnet or reduce the number of faulted interfaces", maxHosts)
	}
	ga := &GatewayAddr{
		Consumer:  consumer,
		Service:   service,
		Interface: iface,
		IP:        a.ipForOffset(a.nextID),
		Port:      port,
		Upstream:  upstream,
	}
	a.nextID++
	a.byKey[ga.key()] = ga
	return ga, nil
}

// lookup returns the address for a triple, if allocated.
func (a *addrAllocator) lookup(consumer, service, iface string) (*GatewayAddr, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ga, ok := a.byKey[GatewayAddr{Consumer: consumer, Service: service, Interface: iface}.key()]
	return ga, ok
}

// forIP returns the address record bound to an IP. The gateway uses it to
// attribute an inbound connection to a (consumer, service, interface) triple.
func (a *addrAllocator) forIP(ip string) (*GatewayAddr, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ga := range a.byKey {
		if ga.IP == ip {
			return ga, true
		}
	}
	return nil, false
}

// all returns every allocated address, ordered by IP so callers (NIC setup,
// reporting) see a stable sequence.
func (a *addrAllocator) all() []*GatewayAddr {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*GatewayAddr, 0, len(a.byKey))
	for _, ga := range a.byKey {
		out = append(out, ga)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}
