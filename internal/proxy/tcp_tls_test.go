package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startTCPEchoTLS is a TLS-wrapped echo server. The proxy dials it
// via proxy.Dial(clientCfg) and the test verifies the bytes survive
// the round trip through both TLS legs. Returns (addr, srvCfg, stop).
func startTCPEchoTLS(t *testing.T) (string, *tls.Config, func()) {
	t.Helper()
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("upstream cert: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), cfg, func() { ln.Close() }
}

// TestTCPProxy_TLSEndToEnd — RFC-038 case for the generic TCP
// plugin: client speaks TLS to the proxy, proxy speaks TLS to the
// upstream, raw bytes survive the round trip in both directions.
func TestTCPProxy_TLSEndToEnd(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startTCPEchoTLS(t)
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newTCPProxy(nil, "custom")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	c, err := tls.Dial("tcp", proxyAddr, &tls.Config{
		RootCAs:    clientPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer c.Close()

	want := []byte("hello-tls-tunnel")
	if _, err := c.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("byte-identity broken: got %q, want %q", got, want)
	}
}

// TestTCPProxy_TLSPrefixRuleStillFires — the headline value of TLS
// for the generic TCP plugin: prefix-based rules see plaintext
// between the two TLS legs and fire as if there were no encryption.
func TestTCPProxy_TLSPrefixRuleStillFires(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startTCPEchoTLS(t)
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newTCPProxy(nil, "custom")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, _ := p.Start(ctx, upstreamAddr)
	defer p.Stop()

	// Respond rule keyed on a byte prefix the client will send.
	// Plaintext match against the TLS-decoded bytes.
	p.AddRule(Rule{
		Method: "BANNED",
		Action: ActionRespond,
		Body:   "BLOCKED",
	})

	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	c, err := tls.Dial("tcp", proxyAddr, &tls.Config{
		RootCAs:    clientPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer c.Close()

	c.Write([]byte("BANNED-payload"))
	got := make([]byte, 7)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "BLOCKED") {
		t.Errorf("rule did not fire: got %q", got)
	}
}

// TestTCPProxy_PlaintextStillWorks — regression check. Without
// SetTLS the plugin retains pre-RFC-038 behavior verbatim. The
// existing TestTCPProxyPassThrough already covers this; we add a
// second test here to keep the TLS-vs-plain contrast explicit in
// the same file.
func TestTCPProxy_PlaintextStillWorks(t *testing.T) {
	srv, _ := net.Listen("tcp", "127.0.0.1:0")
	defer srv.Close()
	go func() {
		c, _ := srv.Accept()
		if c == nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	p := newTCPProxy(nil, "custom")
	// No SetTLS call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, srv.Addr().String())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	want := []byte("ping-plain")
	c.Write(want)
	got := make([]byte, len(want))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("plaintext byte-identity broken: %q", got)
	}
}

// TestTCPProxy_ImplementsTLSAware pins the contract.
func TestTCPProxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*tcpProxy)(nil)
}
