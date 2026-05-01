// Package proxy — bind helpers for transparent fault-injection proxies.
//
// RFC-035: protocol plugins call Listen() instead of net.Listen("tcp",
// "127.0.0.1:0") so the bind side honours the platform default plus
// FAULTBOX_PROXY_BIND override. On Linux the default is "0.0.0.0",
// which is required for container consumers reaching the proxy via
// host.docker.internal (= the docker0 bridge gateway, e.g.
// 172.17.0.1) — loopback isn't reachable from the bridge. On
// macOS/Windows Docker Desktop, host.docker.internal already
// tunnels to host loopback, so the legacy 127.0.0.1 default keeps
// working.
//
// The returned listenAddr is always 127.0.0.1:<port> regardless of
// the bind interface, so host-binary consumers dial loopback
// without translation. Container consumers get
// host.docker.internal:<port> via the Starlark runtime's
// proxyAddrSubstitutionsFor layer.

package proxy

import (
	"fmt"
	"net"
	"os"
	"runtime"
)

// FaultboxProxyBindEnv overrides the bind interface that Listen and
// related helpers use. Useful on bare-metal CI runners where the
// default Linux "0.0.0.0" exposes proxies to the LAN — set to
// "127.0.0.1" to keep them strictly host-local at the cost of
// container reachability.
const FaultboxProxyBindEnv = "FAULTBOX_PROXY_BIND"

// listenHost returns the bind interface for new proxy listeners.
//
// Default: 127.0.0.1 (host loopback) — reachable by host-binary
// consumers and Docker Desktop containers (which tunnel
// host.docker.internal to loopback).
//
// Linux override: 0.0.0.0 — reachable from container consumers on
// Linux Docker, where host.docker.internal resolves to the docker0
// bridge gateway (172.17.0.1) and loopback is not reachable from
// that bridge. This is the default-on-Linux behavior; opt out via
// FAULTBOX_PROXY_BIND=127.0.0.1 if you need strict host-local
// binding (e.g. shared CI runner with public NIC).
func listenHost() string {
	if v := os.Getenv(FaultboxProxyBindEnv); v != "" {
		return v
	}
	if runtime.GOOS == "linux" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// Listen returns a TCP listener bound on the platform-appropriate
// interface (see listenHost) plus a "dialable from this host"
// address — always 127.0.0.1:<port> regardless of the bind side.
// Plugins use this instead of calling net.Listen + ln.Addr().String()
// directly so the bind/dial split is consistent across all 13
// protocols.
func Listen() (net.Listener, string, error) {
	ln, err := net.Listen("tcp", listenHost()+":0")
	if err != nil {
		return nil, "", err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, "", fmt.Errorf("listener address is not TCP: %T", ln.Addr())
	}
	return ln, fmt.Sprintf("127.0.0.1:%d", addr.Port), nil
}

// ListenUDP is the UDP counterpart of Listen. The udp proxy is the
// only plugin that needs it; same semantics — bind side honours
// listenHost, returned addr is 127.0.0.1:<port> for host-side
// dialers.
func ListenUDP() (*net.UDPConn, string, error) {
	laddr, err := net.ResolveUDPAddr("udp", listenHost()+":0")
	if err != nil {
		return nil, "", fmt.Errorf("resolve local: %w", err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, "", err
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return nil, "", fmt.Errorf("listener address is not UDP: %T", conn.LocalAddr())
	}
	return conn, fmt.Sprintf("127.0.0.1:%d", addr.Port), nil
}
