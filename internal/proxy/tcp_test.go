package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startEchoServer stands up a throwaway TCP listener that echoes every
// line it receives with a "real:" prefix, so passthrough and fault
// branches are distinguishable in assertions.
func startEchoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = c.Write([]byte("real:" + line))
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestTCPProxyPassThrough(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	p := newTCPProxy(nil, "echo")
	listenAddr, err := p.Start(context.Background(), echoAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	conn, err := net.DialTimeout("tcp", listenAddr, time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("hi\n"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if line != "real:hi\n" {
		t.Errorf("passthrough got %q, want %q", line, "real:hi\n")
	}
}

func TestTCPProxyDropRule(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	p := newTCPProxy(nil, "echo")
	listenAddr, err := p.Start(context.Background(), echoAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	p.AddRule(Rule{Action: ActionDrop}) // empty prefix → match any

	conn, err := net.DialTimeout("tcp", listenAddr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("hi\n"))
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	// Drop closes the upstream; read should see EOF / closed connection.
	if err == nil && n > 0 {
		t.Errorf("drop rule should close conn, got %d bytes %q", n, buf[:n])
	}
}

func TestTCPProxyRespondRule(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	p := newTCPProxy(nil, "echo")
	listenAddr, err := p.Start(context.Background(), echoAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	p.AddRule(Rule{Action: ActionRespond, Body: "fault-injected\n"})

	conn, _ := net.DialTimeout("tcp", listenAddr, time.Second)
	defer conn.Close()
	_, _ = conn.Write([]byte("anything\n"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(conn)
	if !strings.Contains(string(out), "fault-injected") {
		t.Errorf("respond rule got %q, want fault-injected", out)
	}
	if strings.Contains(string(out), "real:") {
		t.Errorf("respond rule leaked upstream bytes: %q", out)
	}
}

func TestTCPProxyPrefixMatch(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	p := newTCPProxy(nil, "echo")
	listenAddr, err := p.Start(context.Background(), echoAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	// Only match connections whose first bytes start with "ADMIN".
	p.AddRule(Rule{Method: "ADMIN", Action: ActionRespond, Body: "denied\n"})

	// Connection 1: matching prefix → denied.
	c1, _ := net.DialTimeout("tcp", listenAddr, time.Second)
	_, _ = c1.Write([]byte("ADMIN /reload\n"))
	c1.SetReadDeadline(time.Now().Add(1 * time.Second))
	out1, _ := io.ReadAll(c1)
	c1.Close()
	if !strings.Contains(string(out1), "denied") {
		t.Errorf("ADMIN conn: got %q, want denied", out1)
	}

	// Connection 2: non-matching prefix → passthrough.
	c2, _ := net.DialTimeout("tcp", listenAddr, time.Second)
	_, _ = c2.Write([]byte("USER hello\n"))
	c2.SetReadDeadline(time.Now().Add(1 * time.Second))
	br := bufio.NewReader(c2)
	line, _ := br.ReadString('\n')
	c2.Close()
	if !strings.HasPrefix(line, "real:") {
		t.Errorf("USER conn: got %q, want real: prefix (passthrough)", line)
	}
}

func TestTCPProxySupportsProxyRegistered(t *testing.T) {
	if !SupportsProxy("tcp") {
		t.Error("SupportsProxy(tcp) = false, want true after v0.9.6")
	}
	p, err := newProxy("tcp", nil, "svc")
	if err != nil {
		t.Fatalf("newProxy(tcp): %v", err)
	}
	if p.Protocol() != "tcp" {
		t.Errorf("Protocol() = %q, want tcp", p.Protocol())
	}
}
