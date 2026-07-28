//go:build !linux

package netfault

import (
	"fmt"
	"net"
	"runtime"
)

// The packet gateway needs a TUN device, so it is Linux-only at run time.
//
// Everything above the link endpoint — the rule engine, the matcher, the defer
// queue, address allocation — is cross-platform and fully tested on any host
// via link/channel and link/pipe. Only this last hop is stubbed, so a macOS
// developer still gets a complete `make test`.

func newPlatformGateway() platformGateway { return unsupportedGateway{} }

type unsupportedGateway struct{}

func (unsupportedGateway) preflight(*Gateway) error {
	return fmt.Errorf("packet faults require the netstack gateway, which needs a TUN device and is "+
		"Linux-only; this host is %s/%s. Run the spec in the Lima VM (make env-start) or on a Linux host",
		runtime.GOOS, runtime.GOARCH)
}

func (u unsupportedGateway) start(g *Gateway) error { return u.preflight(g) }

func (u unsupportedGateway) listen(g *Gateway, _ *GatewayAddr) (net.Listener, error) {
	return nil, u.preflight(g)
}

func (unsupportedGateway) close() error { return nil }
