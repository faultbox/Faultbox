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

// TestGenerateSelfSignedCert_LoopbackDefaults — the fabricated cert
// must cover at least localhost + 127.0.0.1 so the host-side dial
// address Listen() returns ("127.0.0.1:<port>") works without
// per-test SAN config.
func TestGenerateSelfSignedCert_LoopbackDefaults(t *testing.T) {
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("want 1 cert, got %d", len(cfg.Certificates))
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	wantDNS := []string{"localhost"}
	for _, want := range wantDNS {
		found := false
		for _, got := range leaf.DNSNames {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("cert SAN missing DNS name %q (have %v)", want, leaf.DNSNames)
		}
	}

	foundLoopback := false
	for _, ip := range leaf.IPAddresses {
		if ip.IsLoopback() && ip.To4() != nil {
			foundLoopback = true
			break
		}
	}
	if !foundLoopback {
		t.Errorf("cert SAN missing 127.0.0.1 (have %v)", leaf.IPAddresses)
	}

	if leaf.NotAfter.Before(time.Now().Add(23 * time.Hour)) {
		t.Errorf("cert valid window too short: NotAfter=%s", leaf.NotAfter)
	}
}

// TestGenerateSelfSignedCert_CustomHosts — extra hosts (DNS or IP
// literal) get added to the SAN. Test that a customer adding their
// upstream's hostname to the spec carries through.
func TestGenerateSelfSignedCert_CustomHosts(t *testing.T) {
	cfg, err := GenerateSelfSignedCert([]string{"truck-api.local", "10.1.2.3"})
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	leaf, _ := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])

	hasDNS := false
	for _, n := range leaf.DNSNames {
		if n == "truck-api.local" {
			hasDNS = true
			break
		}
	}
	if !hasDNS {
		t.Errorf("custom DNS name not added: have %v", leaf.DNSNames)
	}

	hasIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "10.1.2.3" {
			hasIP = true
			break
		}
	}
	if !hasIP {
		t.Errorf("custom IP not added: have %v", leaf.IPAddresses)
	}
}

// TestGenerateSelfSignedCert_FreshOnEachCall — every call produces a
// distinct serial / fingerprint so a long-running runtime that
// regenerates per-session avoids serial collisions. Also documents
// the "no persistence" property — clients pinning a fingerprint
// across runs will break, which is the intentional dev-only tradeoff.
func TestGenerateSelfSignedCert_FreshOnEachCall(t *testing.T) {
	a, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("first cert: %v", err)
	}
	b, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("second cert: %v", err)
	}
	leafA, _ := x509.ParseCertificate(a.Certificates[0].Certificate[0])
	leafB, _ := x509.ParseCertificate(b.Certificates[0].Certificate[0])
	if leafA.SerialNumber.Cmp(leafB.SerialNumber) == 0 {
		t.Errorf("serials collided across calls — randomness broken")
	}
}

// TestListenTLS_RejectsNilConfig — defensive: ListenTLS without a
// cfg makes no sense; calling Listen() is the right path. Surface
// the misuse early.
func TestListenTLS_RejectsNilConfig(t *testing.T) {
	_, _, err := ListenTLS(nil)
	if err == nil {
		t.Fatalf("expected error for nil cfg")
	}
}

// TestListenTLS_RoundTrip — a TLS client dialing the listenAddr
// returned by ListenTLS round-trips bytes through tls.Conn.Read /
// Write. Mirrors the canonical Listen passthrough pattern with
// crypto wrapped on top.
func TestListenTLS_RoundTrip(t *testing.T) {
	t.Setenv(FaultboxProxyBindEnv, "")

	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	ln, listenAddr, err := ListenTLS(cfg)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		c.Write(buf)
	}()

	// Build a client cfg that trusts the proxy's auto-generated cert.
	clientCfg := clientCfgFor(t, cfg)
	conn, err := tls.Dial("tcp", listenAddr, clientCfg)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("byte-identity broken: got %q", got)
	}
}

// TestDial_Plaintext — Dial with nil cfg is the plain net.DialTimeout
// equivalent. A trivial echo server confirms Dial returns a usable
// net.Conn.
func TestDial_Plaintext(t *testing.T) {
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	go func() {
		c, _ := srv.Accept()
		if c == nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := Dial(ctx, srv.Addr().String(), nil, 0)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("got %q", buf)
	}
}

// TestDial_TLS — Dial with a non-nil cfg performs the TLS handshake
// against the upstream and returns a *tls.Conn. Run a TLS server
// using the auto-generated cert, point Dial at it, verify a round
// trip works.
func TestDial_TLS(t *testing.T) {
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	srv, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer srv.Close()

	go func() {
		c, _ := srv.Accept()
		if c == nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	clientCfg := clientCfgFor(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := Dial(ctx, srv.Addr().String(), clientCfg, 0)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, ok := conn.(*tls.Conn); !ok {
		t.Errorf("Dial with cfg returned %T, want *tls.Conn", conn)
	}

	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("got %q", buf)
	}
}

// TestDial_TLS_HandshakeTimeout — a TCP server that accepts but
// never sends ServerHello must trigger the handshake deadline.
// Without ctx + SetDeadline plumbing the call would hang past
// Dial's timeout argument.
func TestDial_TLS_HandshakeTimeout(t *testing.T) {
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	// Accept and hold the conn open without doing TLS — the client's
	// handshake will block on ServerHello.
	go func() {
		c, _ := srv.Accept()
		if c == nil {
			return
		}
		// Hold open — never write ServerHello.
		time.Sleep(5 * time.Second)
		c.Close()
	}()

	clientCfg := &tls.Config{InsecureSkipVerify: true}
	start := time.Now()
	_, err = Dial(context.Background(), srv.Addr().String(), clientCfg, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Dial waited %v past timeout — deadline not honoured", elapsed)
	}
	if !strings.Contains(err.Error(), "tls handshake") && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error doesn't look like a handshake timeout: %v", err)
	}
}

// TestDial_TLS_ServerNameDefaultsToTargetHost — when cfg.ServerName
// is empty, Dial fills it from target's host portion so the
// auto-generated cert's localhost SAN matches without callers
// having to set ServerName explicitly.
func TestDial_TLS_ServerNameDefaultsToTargetHost(t *testing.T) {
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	srv, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer srv.Close()
	go func() {
		c, _ := srv.Accept()
		if c == nil {
			return
		}
		defer c.Close()
		// Force the handshake by attempting a read; otherwise an
		// immediate Close races the client's HandshakeContext.
		buf := make([]byte, 1)
		c.Read(buf)
	}()

	// Build a client cfg WITHOUT setting ServerName. Verification
	// must still succeed because Dial picks "127.0.0.1" from the
	// target and the cert's IP-SAN covers it.
	clientCfg := clientCfgFor(t, cfg)
	clientCfg.ServerName = ""

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, srv.Addr().String(), clientCfg, 0)
	if err != nil {
		t.Fatalf("Dial without ServerName: %v", err)
	}
	conn.Close()
}

// clientCfgFor builds a TLS client config that trusts the proxy's
// auto-generated cert by adding it to a per-test root pool.
func clientCfgFor(t *testing.T, serverCfg *tls.Config) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool.AddCert(leaf)
	return &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
}
