package netfault

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// DefaultNICID is the NIC every gateway stack uses. One stack, one NIC —
// multiplexing happens by address, not by interface.
const DefaultNICID tcpip.NICID = 1

// StackConfig describes a gateway stack.
type StackConfig struct {
	// Addrs are the IPv4 addresses this stack answers on, in dotted-quad form.
	// The gateway allocates one per (consumer, target-interface) pair, which
	// is how `source=` targeting resolves without needing the packet's source
	// IP — see RFC-054 decision record M0.2.
	Addrs []string
	// PrefixLen is the subnet mask length for those addresses.
	PrefixLen int
}

// NewStack builds a netstack Stack with ipv4 + tcp/udp/icmp and attaches ep as
// its only NIC.
//
// The link endpoint is supplied by the caller so this works identically over
// link/channel (tests, any OS) and link/fdbased (the Linux TUN gateway). That
// separation is what keeps the whole rule engine testable on a macOS host.
func NewStack(ep stack.LinkEndpoint, cfg StackConfig) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4,
		},
	})
	if err := s.CreateNIC(DefaultNICID, ep); err != nil {
		s.Close()
		return nil, fmt.Errorf("create NIC: %v", err)
	}

	prefix := cfg.PrefixLen
	if prefix == 0 {
		prefix = 16
	}
	for _, a := range cfg.Addrs {
		raw := parseIPv4(a)
		if raw == nil {
			s.Close()
			return nil, fmt.Errorf("invalid IPv4 address %q", a)
		}
		addr := tcpip.AddrFromSlice(raw)
		if err := s.AddProtocolAddress(DefaultNICID, tcpip.ProtocolAddress{
			Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   addr,
				PrefixLen: prefix,
			},
		}, stack.AddressProperties{}); err != nil {
			s.Close()
			return nil, fmt.Errorf("add address %s: %v", a, err)
		}
	}

	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         DefaultNICID,
	}})
	return s, nil
}
