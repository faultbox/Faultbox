package proxy

import (
	"net"
	"runtime"
	"strings"
	"testing"
)

// TestListen_ReturnsLoopbackDialAddr — regardless of platform or
// FAULTBOX_PROXY_BIND override, the address Listen() returns to the
// caller is always 127.0.0.1:<port>. Container consumers get
// host.docker.internal:<port> via the runtime substitution layer;
// this test pins the host-side dial contract.
func TestListen_ReturnsLoopbackDialAddr(t *testing.T) {
	t.Setenv(FaultboxProxyBindEnv, "")
	ln, listenAddr, err := Listen()
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()

	if !strings.HasPrefix(listenAddr, "127.0.0.1:") {
		t.Errorf("listenAddr = %q, want 127.0.0.1:<port> prefix", listenAddr)
	}
	// The reported port must match the listener's actual port.
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is not TCP: %T", ln.Addr())
	}
	want := "127.0.0.1:" + portString(tcp.Port)
	if listenAddr != want {
		t.Errorf("listenAddr = %q, want %q", listenAddr, want)
	}
}

// TestListen_BindsBridgeOnLinux — on Linux the default bind interface
// is 0.0.0.0 so container consumers reaching host.docker.internal
// (= docker0 bridge gateway) can dial the listener. RFC-035 motivation.
func TestListen_BindsBridgeOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific bind default; macOS/Windows keep 127.0.0.1")
	}
	t.Setenv(FaultboxProxyBindEnv, "")
	ln, _, err := Listen()
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	// On Linux, ln.Addr().String() reports the bind interface as
	// "0.0.0.0:<port>" (or "[::]:<port>" if IPv6 dual-stack).
	if !strings.HasPrefix(addr, "0.0.0.0:") && !strings.HasPrefix(addr, "[::]:") {
		t.Errorf("bind addr = %q, want 0.0.0.0:<port> or [::]:<port> on Linux", addr)
	}
}

// TestListen_RespectsEnvOverride — FAULTBOX_PROXY_BIND overrides the
// platform default. Host-binary deployments on shared CI runners can
// pin to 127.0.0.1 to avoid LAN exposure.
func TestListen_RespectsEnvOverride(t *testing.T) {
	t.Setenv(FaultboxProxyBindEnv, "127.0.0.1")
	ln, _, err := Listen()
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("bind addr = %q, want 127.0.0.1:<port> with env override", addr)
	}
}

// TestListenUDP_ReturnsLoopbackDialAddr — same contract as Listen()
// for the UDP proxy.
func TestListenUDP_ReturnsLoopbackDialAddr(t *testing.T) {
	t.Setenv(FaultboxProxyBindEnv, "")
	conn, listenAddr, err := ListenUDP()
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer conn.Close()

	if !strings.HasPrefix(listenAddr, "127.0.0.1:") {
		t.Errorf("listenAddr = %q, want 127.0.0.1:<port> prefix", listenAddr)
	}
}

// TestListen_PassthroughByteIdentity — clients dialing the loopback
// listenAddr land at the same listener whether bind is 0.0.0.0 or
// 127.0.0.1, and bytes flow round-trip cleanly. Mirrors the canonical
// passthrough pattern other proxy plugins use; satisfies #84.
func TestListen_PassthroughByteIdentity(t *testing.T) {
	t.Setenv(FaultboxProxyBindEnv, "")
	ln, listenAddr, err := Listen()
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := c.Read(buf); err != nil {
			return
		}
		c.Write(buf)
	}()

	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial loopback addr: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := conn.Read(got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("byte-identity broken: got %q", got)
	}
}

func portString(p int) string {
	// avoid pulling in strconv just for this — small alloc, only test code
	return netJoinPort(p)
}

// netJoinPort: minimal int → ASCII without strconv.Itoa import noise.
// Test-only helper.
func netJoinPort(p int) string {
	if p == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}
