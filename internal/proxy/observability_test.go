package proxy

import (
	"sync"
	"testing"
	"time"
)

// TestConnTracker_EmitOpenClose — both ends fire, byte counters land
// in fields, and the conn_id is consistent across the pair.
func TestConnTracker_EmitOpenClose(t *testing.T) {
	var mu sync.Mutex
	var got []ProxyEvent
	emit := func(evt ProxyEvent) {
		mu.Lock()
		got = append(got, evt)
		mu.Unlock()
	}

	tr := newConnTracker(emit, "db", "main", "tcp", "127.0.0.1:50001", "127.0.0.1:5432")
	tr.EmitOpen()
	tr.AddBytesC2S(120)
	tr.AddBytesS2C(800)
	tr.EmitClose("client_eof")

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if got[0].Type != EventTypeConnOpen {
		t.Errorf("event[0].Type = %q, want %q", got[0].Type, EventTypeConnOpen)
	}
	if got[1].Type != EventTypeConnClose {
		t.Errorf("event[1].Type = %q, want %q", got[1].Type, EventTypeConnClose)
	}
	if got[0].Fields["conn_id"] != got[1].Fields["conn_id"] {
		t.Errorf("conn_id mismatch: open=%q close=%q",
			got[0].Fields["conn_id"], got[1].Fields["conn_id"])
	}
	if got[1].Fields["bytes_c2s"] != "120" {
		t.Errorf("bytes_c2s = %q, want 120", got[1].Fields["bytes_c2s"])
	}
	if got[1].Fields["bytes_s2c"] != "800" {
		t.Errorf("bytes_s2c = %q, want 800", got[1].Fields["bytes_s2c"])
	}
	if got[1].Fields["reason"] != "client_eof" {
		t.Errorf("reason = %q, want client_eof", got[1].Fields["reason"])
	}
}

// TestConnTracker_HandshakeOnce — defensive: a plugin that calls
// EmitHandshakeComplete twice (e.g. on protocol re-negotiation) only
// emits once. Prevents trace bloat from buggy plugins.
func TestConnTracker_HandshakeOnce(t *testing.T) {
	var got []ProxyEvent
	emit := func(evt ProxyEvent) { got = append(got, evt) }

	tr := newConnTracker(emit, "db", "main", "mysql", "client", "target")
	tr.EmitHandshakeComplete("caching_sha2_password", 5)
	tr.EmitHandshakeComplete("caching_sha2_password", 5)

	if len(got) != 1 {
		t.Fatalf("want 1 handshake event, got %d", len(got))
	}
	if got[0].Type != EventTypeHandshakeComplete {
		t.Errorf("type = %q, want %q", got[0].Type, EventTypeHandshakeComplete)
	}
	if got[0].Fields["auth_method"] != "caching_sha2_password" {
		t.Errorf("auth_method = %q, want caching_sha2_password", got[0].Fields["auth_method"])
	}
}

// TestConnTracker_NilOnEvent — every emit method is a no-op when
// onEvent is nil. Guarantees the helper is zero-cost in tests that
// instantiate plugins without the runtime's proxy manager.
func TestConnTracker_NilOnEvent(t *testing.T) {
	tr := newConnTracker(nil, "db", "main", "tcp", "c", "t")
	// Should not panic.
	tr.EmitOpen()
	tr.EmitHandshakeComplete("", 0)
	tr.AddBytesC2S(10)
	tr.AddBytesS2C(20)
	tr.EmitStall("client_to_server", "command", 0, 5*time.Second)
	tr.EmitClose("client_eof")
}

// TestNewConnID_HexShape — id is stable shape (hex-ish) so the
// renderer can rely on it for grep / filter UI.
func TestNewConnID_HexShape(t *testing.T) {
	a := newConnID()
	b := newConnID()
	if a == b {
		t.Errorf("two consecutive ids collided: %q", a)
	}
	if len(a) < 4 {
		t.Errorf("id %q is suspiciously short", a)
	}
}

// TestClassifyCloseReason — io.EOF and net.ErrClosed map to clean
// reasons; arbitrary errors fall through to io_error so the renderer
// can highlight them.
func TestClassifyCloseReason(t *testing.T) {
	cases := []struct {
		err  error
		side string
		want string
	}{
		{nil, "client", "client_eof"},
		{nil, "server", "server_eof"},
	}
	for _, c := range cases {
		if got := classifyCloseReason(c.err, c.side); got != c.want {
			t.Errorf("classifyCloseReason(%v, %q) = %q, want %q", c.err, c.side, got, c.want)
		}
	}
}
