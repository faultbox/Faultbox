package netfault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/pipe"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// These tests run two real netstack instances connected back to back by
// link/pipe, with the FaultEndpoint spliced into the server side. Real TCP —
// handshake, retransmits, windows — flows through the rule engine.
//
// None of it touches the OS network stack, so the whole thing runs on macOS
// with no privileges. That is the property that makes "confident nothing is
// broken after every milestone" affordable.

const (
	clientAddr = "10.0.0.1"
	serverAddr = "10.0.0.2"
	testPort   = 8080
)

type e2eHarness struct {
	client   *stack.Stack
	server   *stack.Stack
	fe       *FaultEndpoint
	events   chan Event
	listener *gonet.TCPListener
}

func newE2E(t *testing.T, opts Options) *e2eHarness {
	t.Helper()

	epClient, epServer := pipe.New("", "", 1500)

	events := make(chan Event, 256)
	userOnEvent := opts.OnEvent
	opts.OnEvent = func(e Event) {
		if userOnEvent != nil {
			userOnEvent(e)
		}
		select {
		case events <- e:
		default:
		}
	}
	fe := New(epServer, opts)

	cs, err := NewStack(epClient, StackConfig{Addrs: []string{clientAddr}, PrefixLen: 24})
	if err != nil {
		t.Fatalf("client stack: %v", err)
	}
	ss, err := NewStack(fe, StackConfig{Addrs: []string{serverAddr}, PrefixLen: 24})
	if err != nil {
		t.Fatalf("server stack: %v", err)
	}

	// pipe endpoints are L3 here (no ethernet), so no ARP is needed.
	ln, err := gonet.ListenTCP(ss, tcpip.FullAddress{
		NIC:  DefaultNICID,
		Addr: tcpip.AddrFromSlice(parseIPv4(serverAddr)),
		Port: testPort,
	}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h := &e2eHarness{client: cs, server: ss, fe: fe, events: events, listener: ln}
	t.Cleanup(func() {
		ln.Close()
		fe.Close()
		cs.Close()
		ss.Close()
	})
	return h
}

// serveEcho accepts one connection and echoes a fixed response.
func (h *e2eHarness) serveEcho(t *testing.T, response string) {
	t.Helper()
	go func() {
		c, err := h.listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(buf); err != nil {
			return
		}
		io.WriteString(c, response)
	}()
}

func (h *e2eHarness) dial(ctx context.Context) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, h.client, tcpip.FullAddress{
		NIC:  DefaultNICID,
		Addr: tcpip.AddrFromSlice(parseIPv4(serverAddr)),
		Port: testPort,
	}, ipv4.ProtocolNumber)
}

// TestEndToEndPassthrough proves the harness itself works and that a
// FaultEndpoint with no rules is transparent to real TCP.
func TestEndToEndPassthrough(t *testing.T) {
	h := newE2E(t, Options{})
	h.serveEcho(t, "pong")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := h.dial(ctx)
	if err != nil {
		t.Fatalf("dial through passthrough endpoint: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "pong" {
		t.Errorf("response = %q, want %q", got, "pong")
	}
}

// TestEndToEndSynDropBlocksHandshake is RFC-054 scenario 1 in miniature: drop
// the SYN and the connection never establishes — with no RST, so the client
// hangs rather than getting a clean refusal. That distinction is the whole
// point of packet-level faults.
func TestEndToEndSynDropBlocksHandshake(t *testing.T) {
	h := newE2E(t, Options{})
	h.serveEcho(t, "pong")

	set, clear, err := ParseFlagSpec("SYN,!ACK")
	if err != nil {
		t.Fatalf("ParseFlagSpec: %v", err)
	}
	h.fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Label:  "blackhole-syn",
		Match:  Match{Dir: dirPtr(DirC2S), FlagsSet: set, FlagsClear: clear},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	conn, err := h.dial(ctx)
	if err == nil {
		conn.Close()
		t.Fatal("handshake completed despite the SYN being dropped")
	}
	elapsed := time.Since(start)

	// A refusal would come back immediately; a blackhole makes the client wait.
	if elapsed < 500*time.Millisecond {
		t.Errorf("dial failed after only %v — that looks like a refusal, not a blackhole (%v)", elapsed, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Errorf("got a connection-refused error (%v); a dropped SYN must not produce one", err)
	}

	select {
	case e := <-h.events:
		if e.Action != ActionDrop || e.RuleLabel != "blackhole-syn" {
			t.Errorf("unexpected event: %+v", e)
		}
	default:
		t.Error("no packet event emitted for the dropped SYN")
	}
}

// TestEndToEndDelayDoesNotBreakConnection: a delayed handshake still
// completes. This is what separates delay from drop at the TCP level.
func TestEndToEndDelayDoesNotBreakConnection(t *testing.T) {
	h := newE2E(t, Options{})
	h.serveEcho(t, "pong")

	h.fe.SetRules(mustRules(t, &Rule{
		Action: ActionDelay,
		Delay:  40 * time.Millisecond,
		Match:  Match{Dir: dirPtr(DirC2S)},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := h.dial(ctx)
	if err != nil {
		t.Fatalf("dial with delayed packets: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read through delayed path: %v", err)
	}
	if got := string(buf[:n]); got != "pong" {
		t.Errorf("response = %q, want %q", got, "pong")
	}
}

// TestEndToEndResetTearsDownConnection exercises ActionReset against a real
// peer: the client must observe the connection dying, not hang.
func TestEndToEndResetTearsDownConnection(t *testing.T) {
	h := newE2E(t, Options{})
	h.serveEcho(t, "pong")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := h.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Reset only data-carrying segments, so the handshake survives.
	h.fe.SetRules(mustRules(t, &Rule{
		Action: ActionReset,
		Label:  "mid-stream-rst",
		Match:  Match{Dir: dirPtr(DirC2S), LenGT: 0, HasLenGT: true},
	}))

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("read succeeded despite a mid-stream RST")
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Errorf("client timed out instead of seeing the reset: %v", err)
	}
}

// TestEndToEndUnfaultedTrafficUnaffected: a rule scoped to one port must not
// disturb another flow through the same endpoint.
func TestEndToEndUnfaultedTrafficUnaffected(t *testing.T) {
	h := newE2E(t, Options{})
	h.serveEcho(t, "pong")

	h.fe.SetRules(mustRules(t, &Rule{
		Action: ActionDrop,
		Match:  Match{Port: 9999}, // nothing in this test uses 9999
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := h.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	io.WriteString(conn, "ping")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "pong" {
		t.Errorf("response = %q, want %q", got, "pong")
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

var _ = fmt.Sprintf
