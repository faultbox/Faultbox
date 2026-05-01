// RFC-034 — proxy traffic observability.
//
// Emits four new event families through the existing OnProxyEvent
// hook so the bundle's report can show connection lifecycle, byte
// flow, and stall conditions at the proxy layer:
//
//   - proxy_conn_open          — accepted client + dialed upstream
//   - proxy_conn_close         — connection done; duration + byte counts
//   - proxy_handshake_complete — protocol-aware proxies only
//   - proxy_stall              — read direction blocked ≥ threshold
//
// Pre-RFC-034, a customer chasing a proxy-forwarding bug saw
// `proxy_started → 60 seconds of silence → exit_code=2`. Diagnosis
// required SUT-side instrumentation (truck-api / Freight, 2026-04-28).
// With these events, the bundle is self-diagnosing for the
// proxy-forwarding class.

package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Event type constants. New emissions set ProxyEvent.Type to one of
// these so the runtime callback maps them to a distinct event-log
// type (vs the legacy empty-Type → "proxy" path).
const (
	EventTypeConnOpen           = "proxy_conn_open"
	EventTypeConnClose          = "proxy_conn_close"
	EventTypeHandshakeComplete  = "proxy_handshake_complete"
	EventTypeStall              = "proxy_stall"
)

// Stall thresholds. RFC §"Open Question 1" — two-tier was preferred
// (warn at 5s, second event at 30s) over a single configurable
// threshold. Both can be overridden per-connTracker.
const (
	DefaultStallThreshold       = 5 * time.Second
	DefaultStallExtendThreshold = 30 * time.Second
)

// connTracker holds per-connection lifecycle state used by the
// per-plugin observability emit sites. Plugins create one in
// handleConn (after a successful upstream dial) and call EmitOpen,
// EmitHandshake, EmitClose at the appropriate phase boundaries.
//
// Byte counters are exposed via Wrap{Client,Server}Reader / Writer —
// callers that use io.Copy can wrap the readers/writers once and let
// counters update naturally; callers that read in framed chunks
// (mysql, postgres) call AddBytesC2S / S2C explicitly per packet.
type connTracker struct {
	id        string
	onEvent   OnProxyEvent
	service   string
	iface     string
	protocol  string
	client    string
	target    string
	startedAt time.Time

	bytesC2S      atomic.Int64
	bytesS2C      atomic.Int64
	handshakeDone atomic.Bool

	stallThreshold       time.Duration
	stallExtendThreshold time.Duration
}

// newConnTracker creates a tracker; emit-side calls remain no-ops if
// onEvent is nil so tests / non-instrumented manager constructions
// stay zero-cost.
func newConnTracker(onEvent OnProxyEvent, service, iface, protocol, client, target string) *connTracker {
	return &connTracker{
		id:                   newConnID(),
		onEvent:              onEvent,
		service:              service,
		iface:                iface,
		protocol:             protocol,
		client:               client,
		target:               target,
		startedAt:            time.Now(),
		stallThreshold:       DefaultStallThreshold,
		stallExtendThreshold: DefaultStallExtendThreshold,
	}
}

// newConnID returns a short hex identifier for cross-event correlation
// in the bundle. 8 hex chars (4 bytes of randomness) — collision risk
// per test is essentially zero at typical connection counts.
func newConnID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based id — never returning empty so
		// downstream filters can rely on the field's presence.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// EmitOpen fires proxy_conn_open immediately after accept + upstream
// dial succeed, so the timeline marker lands at the right wall-clock
// instant.
func (t *connTracker) EmitOpen() {
	if t == nil || t.onEvent == nil {
		return
	}
	t.onEvent(ProxyEvent{
		Type:     EventTypeConnOpen,
		Protocol: t.protocol,
		To:       t.service,
		Fields: map[string]string{
			"interface": t.iface,
			"protocol":  t.protocol,
			"client":    t.client,
			"target":    t.target,
			"conn_id":   t.id,
		},
	})
}

// EmitHandshakeComplete fires once per connection for protocol-aware
// proxies (mysql, postgres, redis with HELLO, grpc, http2 with TLS).
// Pure passthrough plugins (tcp, udp) skip this entirely — there's
// no handshake boundary to mark.
//
// authMethod is protocol-specific (mysql: "caching_sha2_password",
// postgres: "scram-sha-256" or empty for trust-auth, etc.). Empty is
// fine for protocols without per-connection auth negotiation.
func (t *connTracker) EmitHandshakeComplete(authMethod string, rounds int) {
	if t == nil || t.onEvent == nil {
		return
	}
	if !t.handshakeDone.CompareAndSwap(false, true) {
		// Plugin called twice on the same conn — defensive no-op.
		return
	}
	fields := map[string]string{
		"interface":   t.iface,
		"protocol":    t.protocol,
		"conn_id":     t.id,
		"duration_ms": fmt.Sprintf("%d", time.Since(t.startedAt).Milliseconds()),
	}
	if authMethod != "" {
		fields["auth_method"] = authMethod
	}
	if rounds > 0 {
		fields["rounds"] = fmt.Sprintf("%d", rounds)
	}
	t.onEvent(ProxyEvent{
		Type:     EventTypeHandshakeComplete,
		Protocol: t.protocol,
		To:       t.service,
		Fields:   fields,
	})
}

// EmitClose fires when the connection terminates. Reasons:
//
//   - "client_eof"     — client closed the connection cleanly
//   - "server_eof"     — upstream closed first
//   - "context_cancel" — proxy shutting down (test end, ctx cancel)
//   - "io_error"       — read or write error on either side
//   - "stall_timeout"  — closed because a stall extended past threshold
//   - "rule_drop"      — fault rule dropped the connection
func (t *connTracker) EmitClose(reason string) {
	if t == nil || t.onEvent == nil {
		return
	}
	t.onEvent(ProxyEvent{
		Type:     EventTypeConnClose,
		Protocol: t.protocol,
		To:       t.service,
		Fields: map[string]string{
			"interface":   t.iface,
			"conn_id":     t.id,
			"duration_ms": fmt.Sprintf("%d", time.Since(t.startedAt).Milliseconds()),
			"bytes_c2s":   fmt.Sprintf("%d", t.bytesC2S.Load()),
			"bytes_s2c":   fmt.Sprintf("%d", t.bytesS2C.Load()),
			"reason":      reason,
		},
	})
}

// EmitStall fires when one direction has had bytes pending without
// progress for ≥ stallThreshold. Plugins call this at most twice per
// direction per connection (warn at 5s, then again at 30s if still
// stalled) to keep trace bloat bounded — RFC §"Per-event volume
// control".
//
// direction is "client_to_server" or "server_to_client".
// phase is "handshake" or "command".
func (t *connTracker) EmitStall(direction, phase string, pendingBytes int, stalledFor time.Duration) {
	if t == nil || t.onEvent == nil {
		return
	}
	t.onEvent(ProxyEvent{
		Type:     EventTypeStall,
		Protocol: t.protocol,
		To:       t.service,
		Fields: map[string]string{
			"interface":     t.iface,
			"conn_id":       t.id,
			"direction":     direction,
			"phase":         phase,
			"pending_bytes": fmt.Sprintf("%d", pendingBytes),
			"stalled_ms":    fmt.Sprintf("%d", stalledFor.Milliseconds()),
		},
	})
}

// AddBytesC2S records bytes that flowed client → server. Used by
// plugins that read in framed chunks (mysql, postgres, redis, kafka)
// where wrapping io.Copy isn't practical.
func (t *connTracker) AddBytesC2S(n int) {
	if t == nil {
		return
	}
	t.bytesC2S.Add(int64(n))
}

// AddBytesS2C records bytes that flowed server → client.
func (t *connTracker) AddBytesS2C(n int) {
	if t == nil {
		return
	}
	t.bytesS2C.Add(int64(n))
}

// WrapClientReader wraps a net.Conn (the client side) so reads update
// the C2S byte counter automatically. Plugins doing pure io.Copy
// passthrough use this; callers reading in framed chunks use
// AddBytesC2S directly.
//
// Returns the wrapped reader; the underlying Conn is not closed by
// this wrapper (caller still owns lifecycle).
func (t *connTracker) WrapClientReader(r io.Reader) io.Reader {
	if t == nil {
		return r
	}
	return &countingReader{r: r, n: &t.bytesC2S}
}

// WrapServerReader wraps the upstream side; reads update S2C.
func (t *connTracker) WrapServerReader(r io.Reader) io.Reader {
	if t == nil {
		return r
	}
	return &countingReader{r: r, n: &t.bytesS2C}
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

// classifyCloseReason maps a read/write error to one of the EmitClose
// reason strings. Standard-library io.EOF + net.ErrClosed mean the
// peer closed cleanly; everything else is "io_error".
func classifyCloseReason(err error, side string) string {
	if err == nil || err == io.EOF {
		if side == "client" {
			return "client_eof"
		}
		return "server_eof"
	}
	// net.ErrClosed surfaces when the listener / conn was closed by
	// our side (Stop / context cancel). Don't treat as io_error.
	if ne, ok := err.(*net.OpError); ok {
		if ne.Err != nil && ne.Err.Error() == "use of closed network connection" {
			return "context_cancel"
		}
	}
	return "io_error"
}

// HTTPConnStateTracker wires RFC-034 connection-lifecycle events into
// any net/http.Server-based proxy plugin (http, http2, clickhouse).
// Standard library exposes a ConnState callback firing on
// StateNew / StateActive / StateIdle / StateClosed transitions; we map
// StateNew → proxy_conn_open and StateClosed → proxy_conn_close.
// First StateActive is the connection-ready beat (HTTP has no auth
// handshake; first byte of first request is the closest analogue).
//
// Byte counting at the http.Server layer is not exposed cleanly — the
// standard library reads/writes through the listener for us. v1 wires
// lifecycle + handshake only; bytes_c2s / bytes_s2c report 0 for
// HTTP-family plugins. Follow-up: wrap the underlying net.Conn via a
// custom Listener that returns counting Conns.
type HTTPConnStateTracker struct {
	mu       sync.Mutex
	trackers map[net.Conn]*connTracker
	onEvent  OnProxyEvent
	service  string
	iface    string
	protocol string
	target   string
}

// NewHTTPConnStateTracker constructs the tracker map. Plugins call
// `tracker.ConnState` and assign it to `http.Server.ConnState`.
func NewHTTPConnStateTracker(onEvent OnProxyEvent, service, iface, protocol, target string) *HTTPConnStateTracker {
	return &HTTPConnStateTracker{
		trackers: make(map[net.Conn]*connTracker),
		onEvent:  onEvent,
		service:  service,
		iface:    iface,
		protocol: protocol,
		target:   target,
	}
}

// ConnState is the http.Server.ConnState handler. Hook it on the
// server before Serve(): `srv.ConnState = tracker.ConnState`.
func (h *HTTPConnStateTracker) ConnState(c net.Conn, state http.ConnState) {
	if h == nil || h.onEvent == nil {
		return
	}
	switch state {
	case http.StateNew:
		t := newConnTracker(h.onEvent, h.service, h.iface, h.protocol,
			c.RemoteAddr().String(), h.target)
		t.EmitOpen()
		h.mu.Lock()
		h.trackers[c] = t
		h.mu.Unlock()
	case http.StateActive:
		h.mu.Lock()
		t := h.trackers[c]
		h.mu.Unlock()
		if t != nil {
			// Idempotent — handshakeDone CAS makes subsequent
			// StateActive transitions on keep-alive connections no-ops.
			t.EmitHandshakeComplete("", 1)
		}
	case http.StateClosed, http.StateHijacked:
		h.mu.Lock()
		t := h.trackers[c]
		delete(h.trackers, c)
		h.mu.Unlock()
		if t != nil {
			reason := "client_eof"
			if state == http.StateHijacked {
				reason = "context_cancel"
			}
			t.EmitClose(reason)
		}
	}
}
