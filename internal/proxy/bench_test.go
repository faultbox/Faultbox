package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Benchmarks quantifying RFC-024 pass-through overhead for v0.9.6. Each
// Benchmark* function measures a single request-response cycle against
// a real upstream via (a) a direct TCP/HTTP dial and (b) a pre-started
// Faultbox proxy in passthrough mode (no rules installed). The numbers
// back up the RFC claim that "no rules ⇒ byte-forward ⇒ negligible
// overhead" with an actual delta per protocol.
//
// Run with:
//
//	go test -bench=. -benchmem -run=^$ ./internal/proxy/
//
// Results are protocol-dependent but give a sanity-check floor for any
// future regression (a 10x slowdown would stick out immediately).

// BenchmarkHTTPDirect is the baseline: client → real HTTP upstream, no
// proxy in between. Serves a 32-byte JSON response — small enough that
// overhead dominates payload, which is what we want to measure.
func BenchmarkHTTPDirect(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":1700000000}`))
	}))
	defer upstream.Close()

	client := upstream.Client()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(upstream.URL + "/ping")
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkHTTPThroughProxy: client → Faultbox HTTP proxy (passthrough) →
// real upstream. Delta vs BenchmarkHTTPDirect is the per-request cost of
// the proxy parsing the wire format and reverse-proxying.
func BenchmarkHTTPThroughProxy(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":1700000000}`))
	}))
	defer upstream.Close()

	p := newHTTPProxy(nil, "bench")
	listen, err := p.Start(context.Background(), strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		b.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	client := &http.Client{}
	url := "http://" + listen + "/ping"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkTCPDirect: client → echo upstream, raw TCP. Each iteration
// opens a connection, writes one line, reads one line, closes. Connection
// open/close dominates — the TCP proxy overhead should be < 10% of that.
func BenchmarkTCPDirect(b *testing.B) {
	upstreamAddr, stop := startBenchEcho(b)
	defer stop()
	benchTCPLoop(b, upstreamAddr)
}

// BenchmarkTCPThroughProxy: client → Faultbox TCP proxy (passthrough) →
// echo upstream. Measures the overhead of the prefix-peek + io.Copy
// splice in v0.9.6's new TCP proxy.
func BenchmarkTCPThroughProxy(b *testing.B) {
	upstreamAddr, stop := startBenchEcho(b)
	defer stop()

	p := newTCPProxy(nil, "bench")
	listen, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		b.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()
	benchTCPLoop(b, listen)
}

// BenchmarkRedisDirect / BenchmarkRedisThroughProxy quantify the cost of
// the RESP-parsing proxy on a simple GET command path. Redis is a good
// stand-in for the other parsed-wire protocols (Memcached, NATS): small
// verbs, many requests per connection, pure text parsing.
func BenchmarkRedisDirect(b *testing.B) {
	upstreamAddr, stop := startBenchRedisStub(b)
	defer stop()
	benchRedisLoop(b, upstreamAddr)
}

func BenchmarkRedisThroughProxy(b *testing.B) {
	upstreamAddr, stop := startBenchRedisStub(b)
	defer stop()

	p := newRedisProxy(nil, "bench")
	listen, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		b.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()
	benchRedisLoop(b, listen)
}

// ---- helpers ------------------------------------------------------------

func startBenchEcho(b *testing.B) (string, func()) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// benchTCPLoop keeps a single persistent connection and pingpongs one
// line per iteration. Matches how keepalive clients actually behave and
// avoids darwin ephemeral-port exhaustion from per-iteration dial/close.
func benchTCPLoop(b *testing.B, addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			b.Fatal(err)
		}
		if _, err := br.ReadString('\n'); err != nil {
			b.Fatal(err)
		}
	}
}

// startBenchRedisStub is a minimal RESP-speaking server: it answers every
// GET with "$2\r\nOK\r\n". Covers the proxy's parsing hot path without
// pulling in a real Redis.
func startBenchRedisStub(b *testing.B) (string, func()) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
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
					// Drain one RESP array (crude — just read until we've
					// consumed the bulk-string terminator of the key).
					if _, err := br.ReadString('\n'); err != nil { // *N
						return
					}
					// 2 bulk strings per command (cmd + key).
					for i := 0; i < 4; i++ {
						if _, err := br.ReadString('\n'); err != nil {
							return
						}
					}
					if _, err := c.Write([]byte("$2\r\nOK\r\n")); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func benchRedisLoop(b *testing.B, addr string) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fmt.Fprintf(conn, "*2\r\n$3\r\nGET\r\n$5\r\nkey%02d\r\n", i%100); err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 16)
		if _, err := conn.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
