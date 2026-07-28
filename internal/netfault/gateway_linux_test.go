//go:build linux

package netfault

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Linux integration for the packet gateway. These are the tests that prove
// real traffic crosses the FaultEndpoint — everything else in this package
// runs over link/channel and link/pipe.
//
// They need CAP_NET_ADMIN to create a TUN device, so they skip unless running
// as root. In the Lima VM: sudo -E go test ./internal/netfault/ -run Gateway.

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs CAP_NET_ADMIN to create a TUN device; run as root (see package docs)")
	}
}

// testGateway builds a gateway on a uniquely-named device so concurrent or
// interrupted runs cannot collide on faultbox0.
func testGateway(t *testing.T, cfg GatewayConfig) *Gateway {
	t.Helper()
	if cfg.Device == "" {
		cfg.Device = fmt.Sprintf("fbtest%d", os.Getpid()%10000)
	}
	if cfg.Subnet == "" {
		cfg.Subnet = "10.99.0.0/16"
	}
	g, err := NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	t.Cleanup(func() {
		g.Close()
		// Remove the device even if the test failed mid-way; a leaked TUN
		// breaks the next run with "device busy".
		exec.Command("ip", "link", "del", cfg.Device).Run()
	})
	return g
}

func TestGatewayStartsAndTearsDown(t *testing.T) {
	requireRoot(t)
	g := testGateway(t, GatewayConfig{})

	if _, err := g.Allocate("api", "db", "main", 15432, "127.0.0.1:15432"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := net.InterfaceByName(g.Device()); err != nil {
		t.Fatalf("TUN device %s was not created: %v", g.Device(), err)
	}
	if g.Endpoint() == nil {
		t.Error("FaultEndpoint is nil after Start")
	}

	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// The device is removed by Destroy, invoked through the platform layer on
	// teardown; assert we do not leave a half-configured interface behind.
	if lp, ok := g.platform.(*linuxGateway); ok {
		lp.destroy(g)
	}
	if _, err := net.InterfaceByName(g.Device()); err == nil {
		t.Errorf("TUN device %s survived teardown", g.Device())
	}
}

// TestGatewayCapturesRealTraffic is the headline M4 assertion: a real TCP
// connection from the host reaches a netstack listener through the TUN, and
// every segment crosses the FaultEndpoint.
func TestGatewayCapturesRealTraffic(t *testing.T) {
	requireRoot(t)

	var mu sync.Mutex
	var events []Event
	g := testGateway(t, GatewayConfig{
		OnEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})

	ga, err := g.Allocate("api", "echo", "main", 18080, "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ln, err := g.Listen(ga)
	if err != nil {
		t.Fatalf("Listen on %s: %v", ga.Addr(), err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				n, _ := c.Read(buf)
				io.WriteString(c, "echo:"+string(buf[:n]))
			}(c)
		}
	}()

	conn, err := net.DialTimeout("tcp", ga.Addr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s through the gateway: %v", ga.Addr(), err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Errorf("response = %q, want %q", got, "echo:ping")
	}
}

// TestGatewayDropBlocksRealConnection proves the fault actually bites, and
// that a dropped SYN produces a hang rather than a refusal — the half-open
// blackhole that today's proxy `drop` cannot reproduce.
func TestGatewayDropBlocksRealConnection(t *testing.T) {
	requireRoot(t)
	g := testGateway(t, GatewayConfig{})

	ga, err := g.Allocate("api", "echo", "main", 18081, "")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ln, err := g.Listen(ga)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	set, clear, err := ParseFlagSpec("SYN,!ACK")
	if err != nil {
		t.Fatalf("ParseFlagSpec: %v", err)
	}
	if err := g.InstallRules("", "echo", "main", []*Rule{{
		Action: ActionDrop,
		Label:  "blackhole-syn",
		Match:  Match{Dir: dirPtr(DirC2S), FlagsSet: set, FlagsClear: clear},
	}}); err != nil {
		t.Fatalf("InstallRules: %v", err)
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", ga.Addr(), 1500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		t.Fatal("connected despite the SYN being dropped")
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Errorf("got connection refused (%v); a dropped SYN must hang, not refuse", err)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("dial failed after %v — too fast to be a blackhole", elapsed)
	}

	// And with the rule cleared, the same address works again.
	if err := g.ClearRules("", "echo", "main"); err != nil {
		t.Fatalf("ClearRules: %v", err)
	}
	conn2, err := net.DialTimeout("tcp", ga.Addr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial after clearing the rule: %v", err)
	}
	conn2.Close()
}

// TestGatewayIgnoresHostChatter: a routed subnet also receives unrelated host
// traffic (mDNS to 224.0.0.251 was observed during the M0.2 spike). Rules must
// not fire on it and it must not appear as SUT traffic.
func TestGatewayIgnoresHostChatter(t *testing.T) {
	requireRoot(t)

	var mu sync.Mutex
	var events []Event
	g := testGateway(t, GatewayConfig{
		OnEvent: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	ga, _ := g.Allocate("api", "echo", "main", 18082, "")
	if err := g.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A rule scoped to our port only.
	if err := g.InstallRules("", "echo", "main", []*Rule{{
		Action: ActionDrop,
		Match:  Match{Port: uint16(ga.Port)},
	}}); err != nil {
		t.Fatalf("InstallRules: %v", err)
	}

	// Send an unrelated datagram into the subnet.
	uc, err := net.Dial("udp", "10.99.0.99:9999")
	if err == nil {
		uc.Write([]byte("unrelated"))
		uc.Close()
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.Protocol == ProtoUDP {
			t.Errorf("a rule fired on unrelated subnet traffic: %+v", e)
		}
	}
}
