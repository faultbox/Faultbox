//go:build linux

package netfault

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func newPlatformGateway() platformGateway { return &linuxGateway{} }

type linuxGateway struct {
	mu      sync.Mutex
	fd      int
	stack   *stack.Stack
	started bool
	// createdDevice records whether we created the TUN, so teardown only
	// removes what it made. Reusing a pre-existing device and then deleting
	// it would be a surprising side effect on the user's host.
	createdDevice bool
}

// preflight checks the three things that actually go wrong, and names the fix
// for each. A missing capability otherwise surfaces as a connection timeout
// several seconds into a test, which is a miserable way to learn you needed
// CAP_NET_ADMIN.
func (l *linuxGateway) preflight(g *Gateway) error {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return fmt.Errorf("packet faults need /dev/net/tun, which is missing (%v); "+
			"load the tun module (modprobe tun) or run in the Lima VM", err)
	}
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("packet faults need read/write access to /dev/net/tun (%v); "+
			"run with CAP_NET_ADMIN (sudo, or docker --cap-add=NET_ADMIN)", err)
	}
	f.Close()

	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
		if strings.TrimSpace(string(b)) != "1" {
			return fmt.Errorf("packet faults need IPv4 forwarding so container traffic can reach %s; "+
				"enable it with: sysctl -w net.ipv4.ip_forward=1", g.cfg.Subnet)
		}
	}
	return nil
}

func (l *linuxGateway) start(g *Gateway) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return nil
	}

	created, err := l.ensureDevice(g)
	if err != nil {
		return err
	}
	l.createdDevice = created

	fd, err := openTUN(g.cfg.Device)
	if err != nil {
		l.teardownDevice(g)
		return err
	}
	l.fd = fd

	// IFF_TUN carries L3 frames, so netstack must not expect an ethernet
	// header. Getting this wrong makes every packet unparseable rather than
	// failing loudly, so it is worth stating.
	ep, err := fdbased.New(&fdbased.Options{
		FDs:            []int{fd},
		MTU:            1500,
		EthernetHeader: false,
	})
	if err != nil {
		unix.Close(fd)
		l.teardownDevice(g)
		return fmt.Errorf("create fdbased endpoint on %s: %w", g.cfg.Device, err)
	}

	fe := New(ep, Options{
		Seed:    g.cfg.Seed,
		OnEvent: g.cfg.OnEvent,
		Decider: g.cfg.Decider,
	})

	addrs := make([]string, 0, 8)
	for _, ga := range g.alloc.all() {
		addrs = append(addrs, ga.IP)
	}
	s, err := NewStack(fe, StackConfig{Addrs: addrs, PrefixLen: prefixLenOf(g.cfg.Subnet)})
	if err != nil {
		fe.Close()
		l.teardownDevice(g)
		return err
	}
	// Accept traffic for any address in the subnet, so an address allocated
	// after Start still works without rebuilding the NIC.
	s.SetSpoofing(DefaultNICID, true)
	s.SetPromiscuousMode(DefaultNICID, true)

	l.stack = s
	l.started = true

	g.mu.Lock()
	g.endpoint = fe
	g.mu.Unlock()
	g.republish()
	return nil
}

func (l *linuxGateway) listen(g *Gateway, ga *GatewayAddr) (net.Listener, error) {
	l.mu.Lock()
	s := l.stack
	l.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("gateway not started")
	}
	raw := parseIPv4(ga.IP)
	if raw == nil {
		return nil, fmt.Errorf("invalid gateway address %q", ga.IP)
	}
	// Add the address if it was allocated after Start.
	_ = s.AddProtocolAddress(DefaultNICID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(raw),
			PrefixLen: prefixLenOf(g.cfg.Subnet),
		},
	}, stack.AddressProperties{})

	ln, err := gonet.ListenTCP(s, tcpip.FullAddress{
		NIC:  DefaultNICID,
		Addr: tcpip.AddrFromSlice(raw),
		Port: uint16(ga.Port),
	}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", ga.Addr(), err)
	}
	return ln, nil
}

func (l *linuxGateway) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		return nil
	}
	l.started = false
	if l.stack != nil {
		l.stack.Close()
		l.stack = nil
	}
	if l.fd > 0 {
		unix.Close(l.fd)
		l.fd = 0
	}
	return nil
}

// ensureDevice creates and configures the TUN interface, reporting whether it
// created it (so teardown only removes what it made).
func (l *linuxGateway) ensureDevice(g *Gateway) (created bool, err error) {
	if _, e := net.InterfaceByName(g.cfg.Device); e == nil {
		// Already present — reuse it, and leave it alone on teardown.
		if err := run("ip", "link", "set", g.cfg.Device, "up"); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := run("ip", "tuntap", "add", "dev", g.cfg.Device, "mode", "tun"); err != nil {
		return false, fmt.Errorf("create TUN %s (needs CAP_NET_ADMIN): %w", g.cfg.Device, err)
	}
	hostCIDR := fmt.Sprintf("%s/%d", g.alloc.hostIP(), prefixLenOf(g.cfg.Subnet))
	if err := run("ip", "addr", "add", hostCIDR, "dev", g.cfg.Device); err != nil {
		_ = run("ip", "link", "del", g.cfg.Device)
		return false, fmt.Errorf("assign %s to %s: %w", hostCIDR, g.cfg.Device, err)
	}
	if err := run("ip", "link", "set", g.cfg.Device, "up"); err != nil {
		_ = run("ip", "link", "del", g.cfg.Device)
		return false, fmt.Errorf("bring up %s: %w", g.cfg.Device, err)
	}
	return true, nil
}

func (l *linuxGateway) teardownDevice(g *Gateway) {
	if l.createdDevice {
		_ = run("ip", "link", "del", g.cfg.Device)
		l.createdDevice = false
	}
}

// Destroy removes the TUN device. Separate from close() because the device
// outlives the netstack instance and a leaked one breaks the next run with
// "device busy".
func (l *linuxGateway) Destroy(g *Gateway) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.teardownDevice(g)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func prefixLenOf(cidr string) int {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 16
	}
	ones, _ := ipnet.Mask.Size()
	return ones
}

// openTUN attaches to an existing TUN device and returns its fd.
//
// IFF_NO_PI suppresses the 4-byte packet-info prefix; without it every frame
// would arrive with a header netstack does not expect, and parsing would fail
// silently rather than loudly.
func openTUN(name string) (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr struct {
		name  [16]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], name)
	ifr.flags = unix.IFF_TUN | unix.IFF_NO_PI
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
		uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr))); e != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("TUNSETIFF %s: %w", name, e)
	}
	return fd, nil
}
