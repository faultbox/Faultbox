package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/faultbox/Faultbox/internal/proxy/sqlmatch"
)

type mysqlProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newMySQLProxy(onEvent OnProxyEvent, svcName string) *mysqlProxy {
	return &mysqlProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *mysqlProxy) Protocol() string { return "mysql" }

func (p *mysqlProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	ln, listenAddr, err := Listen()
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.listener = ln
	ctx, p.cancel = context.WithCancel(ctx)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.handleConn(ctx, conn)
			}()
		}
	}()

	return listenAddr, nil
}

func (p *mysqlProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// RFC-034: per-connection lifecycle tracker. Emits proxy_conn_open
	// now, proxy_handshake_complete after auth succeeds, and
	// proxy_conn_close in deferred cleanup with byte counts +
	// termination reason. Byte counters update inside the
	// command-phase loop (handshake bytes are small relative to
	// command-phase traffic; v1 reports command-phase bytes only).
	tracker := newConnTracker(p.onEvent, p.svcName, "mysql", "mysql",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()

	if err := p.forwardHandshake(clientConn, serverConn); err != nil {
		closeReason = classifyCloseReason(err, "client")
		return
	}
	tracker.EmitHandshakeComplete("", 0)

	// Proxy command packets.
	for {
		select {
		case <-ctx.Done():
			closeReason = "context_cancel"
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// MySQL packet: 3-byte length (little-endian) + 1-byte sequence + payload.
		header := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		tracker.AddBytesC2S(4)

		payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
		if payloadLen == 0 {
			continue
		}

		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(clientConn, payload); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		tracker.AddBytesC2S(payloadLen)

		// COM_QUERY = 0x03, COM_STMT_PREPARE = 0x16
		if len(payload) > 0 && (payload[0] == 0x03 || payload[0] == 0x16) {
			query := string(payload[1:])
			if handled := p.checkRules(clientConn, header[3], query); handled {
				continue
			}
		}

		// Forward to server.
		if _, err := serverConn.Write(header); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		if _, err := serverConn.Write(payload); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}

		// Forward server response(s) back to client.
		respBytes, err := p.forwardResponse(serverConn, clientConn)
		if err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		tracker.AddBytesS2C(respBytes)
	}
}

func (p *mysqlProxy) checkRules(clientConn net.Conn, seqID byte, query string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		// Query match uses SQL-aware canonicalization so rules keyed on
		// "SELECT * FROM users WHERE id = ?" match drivers' tight output
		// like "select * from users where id=$1;" regardless of case,
		// whitespace, placeholder dialect, or trailing ';'.
		if !sqlmatch.Match(query, rule.Query) {
			continue
		}
		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}

		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}

		switch rule.Action {
		case ActionError:
			errMsg := rule.Error
			if errMsg == "" {
				errMsg = "Injected fault"
			}
			sendMySQLError(clientConn, seqID+1, 1105, errMsg)

			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "mysql",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"query": query, "error": errMsg},
				})
			}
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "mysql",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"query": query, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false

		case ActionDrop:
			clientConn.Close()
			return true
		}
	}
	return false
}

// MySQL packet first-byte markers (server → client).
//
// 0x00 OK_Packet, 0xFE EOF/Auth-Switch-Request, 0xFF ERR_Packet,
// 0x01 AuthMoreData (server-side prefix for caching_sha2_password's
// "perform full authentication" + public-key payloads).
//
// AuthMoreData second byte (caching_sha2_password):
// 0x03 fast_auth_success — server immediately sends OK after, NO
//
//	client packet between, so the proxy must keep reading server.
//
// 0x04 perform_full_authentication — client must respond with either
//
//	a public-key request (0x02) or, under TLS, the cleartext
//	password.
//
// (other) — typically a public-key payload during full-auth; client
//
//	replies with the encrypted password.
//
// Sources:
// - https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_response_packets.html
// - https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_authentication_methods_authentication_caching_sha2_password.html
const (
	mysqlPktOK           = 0x00
	mysqlPktAuthMoreData = 0x01
	// 0xFE is AuthSwitchRequest in the connection phase and EOF in the
	// command phase. Same byte, different meaning depending on where the
	// connection is; both names exist so each call site reads correctly.
	mysqlPktAuthSwitchReq = 0xFE
	mysqlPktEOF           = 0xFE
	// 0xFB introduces a LOCAL INFILE request — the server asking the client
	// to upload a file. The client replies next, so as far as this exchange
	// is concerned it terminates the response.
	mysqlPktLocalInfile      = 0xFB
	mysqlPktERR              = 0xFF
	mysqlSha2FastAuthSuccess = 0x03
)

// forwardHandshake relays packets between client and server until the
// connection-phase exchange terminates with an OK or ERR packet.
//
// Pre-v0.12.14 this was a strict 3-packet exchange: server greeting,
// client handshake response, server OK. That works for
// `mysql_native_password` but breaks for `caching_sha2_password` (the
// MySQL 8 default) when the user isn't in the server's auth cache:
// the server emits AuthMoreData(0x01,0x04 = "perform full auth"),
// then the client requests a public key (0x02), then the server sends
// the key, then the client sends the encrypted password, then the
// server sends OK. That's six packets; the old code returned after
// three and entered the command loop with the auth state machine
// mid-flight, deadlocking client + server.
//
// The fix: loop, peeking at server-side packets to detect terminal
// states (OK / ERR), and at every other server packet send a client
// packet through. Bounded by maxRounds to defend against malformed
// peers.
func (p *mysqlProxy) forwardHandshake(client, server net.Conn) error {
	// 1. Always: server greeting.
	if err := forwardMySQLPacket(server, client); err != nil {
		return fmt.Errorf("server greeting: %w", err)
	}
	// 2. Always: first client handshake response.
	if err := forwardMySQLPacket(client, server); err != nil {
		return fmt.Errorf("client handshake response: %w", err)
	}

	// 3..N. Loop server→client, expecting an alternating client→server
	// reply for any packet the protocol genuinely needs one for. The
	// non-obvious case is caching_sha2_password's "fast_auth_success"
	// path: when the user is already in the server's auth cache (e.g.
	// because seed_db just connected directly), the server sends two
	// server-side packets BACK-TO-BACK — `AuthMoreData(0x01, 0x03)`
	// followed by `OK(0x00)` — with NO client packet in between.
	// Pre-v0.12.15 this looped after the AuthMoreData expecting a
	// client reply that never came, deadlocking until the client's
	// connect timeout fired (Finding H, customer report 2026-04-29).
	//
	// 16 rounds gives generous headroom for any future plugin while
	// staying bounded against a malformed peer.
	const maxRounds = 16
	for i := 0; i < maxRounds; i++ {
		first, second, err := forwardMySQLPacketReturningFirstTwoBytes(server, client)
		if err != nil {
			return fmt.Errorf("server auth response (round %d): %w", i+1, err)
		}
		if first == mysqlPktOK || first == mysqlPktERR {
			return nil
		}
		// caching_sha2_password fast_auth_success: server emits
		// AuthMoreData(0x01, 0x03) immediately followed by OK(0x00)
		// — both server-side, no client packet between. Skip the
		// client read and let the next loop iteration pick up the OK.
		if first == mysqlPktAuthMoreData && second == mysqlSha2FastAuthSuccess {
			continue
		}
		// AuthSwitchRequest (0xFE), AuthMoreData(0x01, 0x04 = "perform
		// full authentication"), and AuthMoreData with a public-key
		// payload all require a client reply.
		if err := forwardMySQLPacket(client, server); err != nil {
			return fmt.Errorf("client auth continuation (round %d): %w", i+1, err)
		}
	}
	return fmt.Errorf("handshake exceeded %d rounds without OK/ERR", maxRounds)
}

// mysqlResponseTimeout bounds a single server read in the command phase.
//
// The client side of the loop has always had a deadline; the server side had
// none, so any mis-parse of the response framing blocked a proxy goroutine
// forever. Stop() waits on the connection WaitGroup, so one stuck goroutine
// hung the entire run in teardown — not even the per-test timeout fired,
// because the test itself had already finished.
//
// A deadline turns that class of bug into a failed test with a legible error
// instead of a hang, which is the only acceptable behaviour for a tool whose
// job is reporting what happened.
// A var, not a const, so tests can shorten it — asserting that the read is
// bounded should not cost 30 seconds of suite time.
var mysqlResponseTimeout = 30 * time.Second

// forwardResponse forwards one COM_* response from server to client.
//
// Returns total bytes read from the server side so the connection-lifecycle
// tracker can update bytes_s2c for proxy_conn_close. Includes packet headers
// and payloads.
//
// # Why this parses the result set rather than guessing
//
// The previous implementation forwarded one packet per loop iteration and then
// peeked with a 100 ms deadline to decide whether more was coming. The peek
// consumed the terminator, so the *next* iteration issued an unconditional,
// deadline-free read for a packet the server would never send — and blocked
// forever. `exec()` was unaffected (a single OK packet returns above), so the
// bug was specific to `query()`: every result-set step through the MySQL proxy
// hung, permanently.
//
// It stayed invisible because the proxy could not reach the command phase at
// all until v0.16.1 fixed the credentials that let a step authenticate. One
// bug was hiding behind another, and neither was visible to a spec that did
// not assert on the result of a step.
//
// The framing (MySQL 8 COM_QUERY):
//
//	OK (0x00) | ERR (0xFF) | LOCAL INFILE (0xFB) → single packet, done
//	otherwise → column count N, then N column definitions, then
//	            [EOF] when the client did not negotiate CLIENT_DEPRECATE_EOF,
//	            then row packets, terminated by EOF (0xFE) or OK (0x00) or ERR
//
// Both EOF styles are handled without knowing the negotiated capability
// flags: a 0x00 terminator is the deprecate-EOF final OK and ends the
// response, while the first 0xFE after the column definitions is the
// column-definition terminator and the second ends the rows.
func (p *mysqlProxy) forwardResponse(server, client net.Conn) (int, error) {
	bytesRead := 0

	readPacket := func() (payload []byte, n int, err error) {
		if err := server.SetReadDeadline(time.Now().Add(mysqlResponseTimeout)); err != nil {
			return nil, 0, err
		}
		defer server.SetReadDeadline(time.Time{})

		header := make([]byte, 4)
		if _, err := io.ReadFull(server, header); err != nil {
			return nil, 0, err
		}
		n = 4
		payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
		payload = make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(server, payload); err != nil {
				return nil, n, err
			}
			n += payloadLen
		}
		if _, err := client.Write(header); err != nil {
			return nil, n, err
		}
		if len(payload) > 0 {
			if _, err := client.Write(payload); err != nil {
				return nil, n, err
			}
		}
		return payload, n, nil
	}

	first, n, err := readPacket()
	bytesRead += n
	if err != nil {
		return bytesRead, err
	}

	// Single-packet responses: OK, ERR, or a LOCAL INFILE request (which the
	// client answers next, so this exchange is over either way).
	if len(first) > 0 && (first[0] == mysqlPktOK || first[0] == mysqlPktERR || first[0] == mysqlPktLocalInfile) {
		return bytesRead, nil
	}
	// An EOF as the very first packet is not a result set either.
	if len(first) > 0 && first[0] == mysqlPktEOF && len(first) < 9 {
		return bytesRead, nil
	}

	// Result set. first is the column-count packet — a length-encoded
	// integer, and for any plausible column count that is its first byte.
	columns := mysqlLenEncInt(first)

	for i := 0; i < columns; i++ {
		_, n, err := readPacket()
		bytesRead += n
		if err != nil {
			return bytesRead, err
		}
	}

	sawColumnDefEOF := false
	for {
		payload, n, err := readPacket()
		bytesRead += n
		if err != nil {
			return bytesRead, err
		}
		if len(payload) == 0 {
			continue
		}
		switch {
		case payload[0] == mysqlPktERR:
			return bytesRead, nil
		case payload[0] == mysqlPktOK:
			// CLIENT_DEPRECATE_EOF: the final packet is an OK, not an EOF.
			return bytesRead, nil
		case payload[0] == mysqlPktEOF && len(payload) < 9:
			if sawColumnDefEOF {
				return bytesRead, nil // end of rows
			}
			sawColumnDefEOF = true // end of column definitions; rows follow
		}
		// Otherwise a row packet — keep going.
	}
}

// mysqlLenEncInt decodes the leading length-encoded integer of a payload.
// Only the first byte matters for realistic column counts; the multi-byte
// forms are decoded anyway so a wide result set is not mis-framed.
func mysqlLenEncInt(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	switch b := payload[0]; {
	case b < 0xFB:
		return int(b)
	case b == 0xFC && len(payload) >= 3:
		return int(payload[1]) | int(payload[2])<<8
	case b == 0xFD && len(payload) >= 4:
		return int(payload[1]) | int(payload[2])<<8 | int(payload[3])<<16
	default:
		return 0
	}
}

func forwardMySQLPacket(src, dst net.Conn) error {
	_, _, err := forwardMySQLPacketReturningFirstTwoBytes(src, dst)
	return err
}

// forwardMySQLPacketReturningFirstTwoBytes forwards one MySQL packet from
// src to dst and returns the first two payload bytes (or zero values
// when the payload has fewer). The caller uses this to drive the
// handshake state machine: the first byte distinguishes
// OK/ERR/AuthSwitch/AuthMoreData; the second byte is needed to
// distinguish caching_sha2_password's `fast_auth_success` (0x01, 0x03)
// — which is followed by another server-side OK with no client reply
// between — from `perform_full_authentication` (0x01, 0x04) and
// public-key payloads, which all expect a client response.
func forwardMySQLPacketReturningFirstTwoBytes(src, dst net.Conn) (byte, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(src, header); err != nil {
		return 0, 0, err
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if _, err := dst.Write(header); err != nil {
		return 0, 0, err
	}
	if payloadLen == 0 {
		return 0, 0, nil
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(src, payload); err != nil {
		return 0, 0, err
	}
	if _, err := dst.Write(payload); err != nil {
		return 0, 0, err
	}
	if payloadLen == 1 {
		return payload[0], 0, nil
	}
	return payload[0], payload[1], nil
}

// sendMySQLError sends a MySQL ERR_Packet.
func sendMySQLError(conn net.Conn, seqID byte, code uint16, msg string) {
	// ERR_Packet: 0xFF + error_code(2) + sql_state_marker('#') + sql_state(5) + message
	payload := make([]byte, 0, 9+len(msg))
	payload = append(payload, 0xFF) // ERR marker
	payload = append(payload, byte(code), byte(code>>8))
	payload = append(payload, '#')
	payload = append(payload, []byte("HY000")...) // generic SQL state
	payload = append(payload, []byte(msg)...)

	header := make([]byte, 4)
	header[0] = byte(len(payload))
	header[1] = byte(len(payload) >> 8)
	header[2] = byte(len(payload) >> 16)
	header[3] = seqID

	conn.Write(header)
	conn.Write(payload)
}

func (p *mysqlProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *mysqlProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *mysqlProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	waitConns(&p.wg, p.onEvent, p.svcName, "mysql")
	return nil
}

// Ensure binary import is used.
var _ = binary.BigEndian
