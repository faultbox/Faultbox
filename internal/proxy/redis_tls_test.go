package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startFakeRedisTLS is the TLS variant of startFakeRedis from
// redis_test.go. The upstream terminates TLS itself (matching how a
// production Redis broker with `tls-port` works), and the proxy
// has to dial it with proxy.Dial + clientCfg.
func startFakeRedisTLS(t *testing.T, reply string) (string, *tls.Config, func()) {
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
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := readRESPArray(br); err != nil {
					return
				}
				_, _ = c.Write([]byte(reply))
			}(c)
		}
	}()
	return ln.Addr().String(), cfg, func() { ln.Close() }
}

// TestRedisProxy_TLSEndToEnd — RFC-038 case for redis: client
// speaks RESP-over-TLS to the proxy, proxy speaks RESP-over-TLS to
// the upstream, plaintext command parsing keeps working in the
// middle so key-glob / command rules still fire.
func TestRedisProxy_TLSEndToEnd(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startFakeRedisTLS(t, "+OK\r\n")
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newRedisProxy(nil, "cache")
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

	if _, err := c.Write([]byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")); err != nil {
		t.Fatalf("write SET: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !strings.HasPrefix(got, "+OK") {
		t.Errorf("reply = %q, want +OK", got)
	}
}

// TestRedisProxy_TLSRuleInjection — fault rule fires inside the TLS
// tunnel. Customer's exact use case: TLS Redis upstream + key-glob
// fault.
func TestRedisProxy_TLSRuleInjection(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := startFakeRedisTLS(t, "+OK\r\n")
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	var ruleHits int32
	p := newRedisProxy(func(evt ProxyEvent) {
		if evt.Action == "error" {
			atomic.AddInt32(&ruleHits, 1)
		}
	}, "cache")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, _ := p.Start(ctx, upstreamAddr)
	defer p.Stop()

	p.AddRule(Rule{
		Command: "GET",
		Key:     "session:*",
		Action:  ActionError,
		Error:   "ERR injected",
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

	if _, err := fmt.Fprintf(c, "*2\r\n$3\r\nGET\r\n$10\r\nsession:42\r\n"); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !strings.Contains(got, "injected") {
		t.Errorf("reply = %q, want injected error", got)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&ruleHits) == 0 {
		t.Errorf("expected at least one error event, got 0")
	}
}

// TestRedisProxy_PlaintextStillWorks — regression check. Without
// SetTLS the plugin retains pre-RFC-038 behavior verbatim. The
// existing redis_test.go tests cover RESP correctness exhaustively;
// this file pins the no-TLS-call code path that earlier tests
// implicitly exercised.
func TestRedisProxy_PlaintextStillWorks(t *testing.T) {
	upstreamAddr, stop := startFakeRedis(t, "+OK\r\n")
	defer stop()

	p := newRedisProxy(nil, "cache")
	// No SetTLS call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(got, "+OK") {
		t.Errorf("plaintext reply = %q, want +OK", got)
	}
}

// TestRedisProxy_ImplementsTLSAware pins the contract.
func TestRedisProxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*redisProxy)(nil)
}
