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

	// Forward handshake: server greeting → client auth → server OK.
	if err := p.forwardHandshake(clientConn, serverConn); err != nil {
		return
	}

	// Proxy command packets.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// MySQL packet: 3-byte length (little-endian) + 1-byte sequence + payload.
		header := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			return
		}

		payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
		if payloadLen == 0 {
			continue
		}

		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(clientConn, payload); err != nil {
			return
		}

		// COM_QUERY = 0x03, COM_STMT_PREPARE = 0x16
		if len(payload) > 0 && (payload[0] == 0x03 || payload[0] == 0x16) {
			query := string(payload[1:])
			if handled := p.checkRules(clientConn, header[3], query); handled {
				continue
			}
		}

		// Forward to server.
		if _, err := serverConn.Write(header); err != nil {
			return
		}
		if _, err := serverConn.Write(payload); err != nil {
			return
		}

		// Forward server response(s) back to client.
		if err := p.forwardResponse(serverConn, clientConn); err != nil {
			return
		}
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
//      client packet between, so the proxy must keep reading server.
// 0x04 perform_full_authentication — client must respond with either
//      a public-key request (0x02) or, under TLS, the cleartext
//      password.
// (other) — typically a public-key payload during full-auth; client
//      replies with the encrypted password.
//
// Sources:
// - https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_response_packets.html
// - https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_authentication_methods_authentication_caching_sha2_password.html
const (
	mysqlPktOK              = 0x00
	mysqlPktAuthMoreData    = 0x01
	mysqlPktAuthSwitchReq   = 0xFE
	mysqlPktERR             = 0xFF
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
	// connect timeout fired (Finding H, Freight 2026-04-29).
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

// forwardResponse forwards MySQL response packets from server to client.
// MySQL responses can be multi-packet (result sets, etc.).
func (p *mysqlProxy) forwardResponse(server, client net.Conn) error {
	// Read first packet to determine response type.
	header := make([]byte, 4)
	if _, err := io.ReadFull(server, header); err != nil {
		return err
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(server, payload); err != nil {
			return err
		}
	}

	// Forward to client.
	if _, err := client.Write(header); err != nil {
		return err
	}
	if _, err := client.Write(payload); err != nil {
		return err
	}

	// If OK (0x00), EOF (0xFE), or Error (0xFF) — done.
	if payloadLen > 0 && (payload[0] == 0x00 || payload[0] == 0xFE || payload[0] == 0xFF) {
		return nil
	}

	// Otherwise it's a result set — forward until EOF marker.
	// Column definitions + rows + EOF.
	for {
		if err := forwardMySQLPacket(server, client); err != nil {
			return err
		}
		// Check for EOF — simplified, just forward a bounded number.
		// In practice we'd parse the column count and track state.
		// For the proxy, forwarding all packets until server stops is sufficient.
		// Use a short read timeout to detect end of response.
		server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		peek := make([]byte, 4)
		n, err := server.Read(peek)
		server.SetReadDeadline(time.Time{}) // reset
		if err != nil || n == 0 {
			return nil // response complete
		}
		// Got more data — forward it.
		payloadLen = int(peek[0]) | int(peek[1])<<8 | int(peek[2])<<16
		client.Write(peek[:n])
		if payloadLen > 0 {
			remaining := make([]byte, payloadLen)
			io.ReadFull(server, remaining)
			client.Write(remaining)
		}
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
	p.wg.Wait()
	return nil
}

// Ensure binary import is used.
var _ = binary.BigEndian
