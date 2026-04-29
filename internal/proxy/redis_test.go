package proxy

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// helloResp3MapResponse is a representative RESP3 reply to `HELLO 3`: a 7-pair
// map whose `modules` value is itself a `*0` array. Mirrors the wire shape the
// customer dumped in the v0.12.15 RED report (Finding J).
const helloResp3MapResponse = "%7\r\n" +
	"$6\r\nserver\r\n$5\r\nredis\r\n" +
	"$7\r\nversion\r\n$5\r\n7.2.0\r\n" +
	"$5\r\nproto\r\n:3\r\n" +
	"$2\r\nid\r\n:42\r\n" +
	"$4\r\nmode\r\n$10\r\nstandalone\r\n" +
	"$4\r\nrole\r\n$6\r\nmaster\r\n" +
	"$7\r\nmodules\r\n*0\r\n"

// pongResp2 is a vanilla RESP2 +PONG reply.
const pongResp2 = "+PONG\r\n"

// startFakeRedis runs a minimal upstream that consumes one client RESP array
// per connection and writes back a fixed reply. Returns the listen addr.
func startFakeRedis(t *testing.T, reply string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
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
	return ln.Addr().String(), func() { ln.Close() }
}

// TestRedisProxy_RESP3_HelloMap is the Finding J regression: HELLO 3 returns a
// RESP3 map (%7) with a nested *0 array; v0.12.15 fell through `%` to default
// and stalled mid-response. v0.12.15.1 must round-trip the full payload.
func TestRedisProxy_RESP3_HelloMap(t *testing.T) {
	upstreamAddr, stop := startFakeRedis(t, helloResp3MapResponse)
	defer stop()

	p := newRedisProxy(nil, "redis")
	addr, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	cl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cl.Close()

	if _, err := cl.Write([]byte("*2\r\n$5\r\nHELLO\r\n$1\r\n3\r\n")); err != nil {
		t.Fatalf("write HELLO 3: %v", err)
	}

	cl.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(cl)
	got, err := readRESPRaw(br)
	if err != nil {
		t.Fatalf("read response (proxy stalled mid-map?): %v", err)
	}

	if string(got) != helloResp3MapResponse {
		t.Fatalf("response truncated or corrupted:\n got (%d bytes): %q\nwant (%d bytes): %q",
			len(got), got, len(helloResp3MapResponse), helloResp3MapResponse)
	}
}

// TestRedisProxy_RESP2_Ping is the no-regression guard for vanilla RESP2 after
// the readRESPRaw switch was widened to cover RESP3.
func TestRedisProxy_RESP2_Ping(t *testing.T) {
	upstreamAddr, stop := startFakeRedis(t, pongResp2)
	defer stop()

	p := newRedisProxy(nil, "redis")
	addr, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	cl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cl.Close()

	if _, err := cl.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}

	cl.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(cl)
	got, err := readRESPRaw(br)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.HasPrefix(string(got), "+PONG") {
		t.Fatalf("expected +PONG, got %q", got)
	}
}

// TestRedisProxy_RESP3_Set covers the `~` aggregate (e.g. SMEMBERS reply in
// RESP3 mode). Same parser pattern as map but N elements not 2N.
func TestRedisProxy_RESP3_Set(t *testing.T) {
	setReply := "~3\r\n$1\r\na\r\n$1\r\nb\r\n$1\r\nc\r\n"
	upstreamAddr, stop := startFakeRedis(t, setReply)
	defer stop()

	p := newRedisProxy(nil, "redis")
	addr, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	cl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cl.Close()

	if _, err := cl.Write([]byte("*2\r\n$8\r\nSMEMBERS\r\n$1\r\nk\r\n")); err != nil {
		t.Fatalf("write SMEMBERS: %v", err)
	}

	cl.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(cl)
	got, err := readRESPRaw(br)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != setReply {
		t.Fatalf("set response mismatch:\n got: %q\nwant: %q", got, setReply)
	}
}

// TestRedisProxy_RESP3_Attribute covers the `|` attribute frame which precedes
// a regular reply; both must round-trip as one logical value.
func TestRedisProxy_RESP3_Attribute(t *testing.T) {
	// |1 attribute (key-popularity → 0.5) followed by a +OK reply.
	attrReply := "|1\r\n$14\r\nkey-popularity\r\n,0.5\r\n+OK\r\n"
	upstreamAddr, stop := startFakeRedis(t, attrReply)
	defer stop()

	p := newRedisProxy(nil, "redis")
	addr, err := p.Start(context.Background(), upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	cl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cl.Close()

	if _, err := cl.Write([]byte("*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")); err != nil {
		t.Fatalf("write GET: %v", err)
	}

	cl.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(cl)
	got, err := readRESPRaw(br)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != attrReply {
		t.Fatalf("attribute+reply mismatch:\n got: %q\nwant: %q", got, attrReply)
	}
}
