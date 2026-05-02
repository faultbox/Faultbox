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
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"
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

// ListenTLS is the TLS-aware variant of Listen. The bind side and
// returned listenAddr behave exactly like Listen — bind interface
// honours listenHost(), addr is always 127.0.0.1:<port>. The
// listener wraps incoming connections in tls.NewListener using the
// provided cfg, so Accept() returns *tls.Conn whose Read/Write speak
// plaintext to the proxy plugin while transmitting ciphertext on
// the wire.
//
// Callers must provide cfg with at least one Certificate (or
// GetCertificate). For dev / test where the proxy runs without an
// externally issued cert, use GenerateSelfSignedCert() to build a
// cfg.
//
// RFC-038 Phase 1 — sibling of Listen rather than an extension to
// keep the 14 existing Listen callsites unchanged. Plugins that
// adopt TLS in Phase 3 switch their listen call from Listen() to
// ListenTLS(cfg) when an interface declares tls=...
func ListenTLS(cfg *tls.Config) (net.Listener, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("ListenTLS: tls.Config required (use Listen for plaintext)")
	}
	ln, listenAddr, err := Listen()
	if err != nil {
		return nil, "", err
	}
	return tls.NewListener(ln, cfg), listenAddr, nil
}

// Dial is the upstream-side companion of Listen / ListenTLS. With a
// nil tlsCfg it behaves like net.DialTimeout("tcp", target, …); with
// a non-nil cfg it negotiates TLS against the upstream and returns
// the *tls.Conn (typed as net.Conn).
//
// The DialTimeout-equivalent applies to both the TCP connect and
// the TLS handshake: the function returns the moment either step
// fails or the deadline expires. ctx cancellation is honoured for
// the TCP step; the TLS handshake observes the same deadline via
// SetDeadline so a stalled handshake does not outlive ctx.
//
// The hostname used for TLS verification (ServerName) follows
// cfg.ServerName when set; otherwise it is derived from target's
// host portion. Plugins typically set ServerName=target-host
// explicitly so the customer's `tls=tls_cert(ca=...)` material
// matches whatever certificate the upstream presents.
//
// RFC-038 Phase 1 — added so plugins can switch their upstream dial
// from net.DialTimeout to proxy.Dial without re-implementing TLS
// per-plugin. Phase 3 migrates plugins one at a time.
func Dial(ctx context.Context, target string, cfg *tls.Config, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := &net.Dialer{}
	tcp, err := d.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", target, err)
	}
	if cfg == nil {
		return tcp, nil
	}

	// Default ServerName to the host portion of target so the cert's
	// SAN/CN match without callers having to spell it out. Callers
	// that need an explicit override (e.g. SNI override for a CDN
	// upstream) set cfg.ServerName themselves and we leave it alone.
	if cfg.ServerName == "" {
		host, _, splitErr := net.SplitHostPort(target)
		if splitErr == nil {
			cfg = cfg.Clone()
			cfg.ServerName = host
		}
	}

	tlsConn := tls.Client(tcp, cfg)
	// Honour the ctx deadline for the handshake too — without this a
	// stalled handshake would block past ctx cancellation.
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("tls handshake %s: %w", target, err)
	}
	// Clear the handshake deadline; further reads/writes use plugin-
	// level deadlines (or none).
	_ = tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
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
