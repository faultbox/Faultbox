package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/faultbox/Faultbox/internal/proxy/sqlmatch"
)

type postgresProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newPostgresProxy(onEvent OnProxyEvent, svcName string) *postgresProxy {
	return &postgresProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *postgresProxy) Protocol() string { return "postgres" }

func (p *postgresProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *postgresProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// RFC-034: per-connection lifecycle tracker. Postgres has a fixed
	// 3-phase handshake (startup → server response loop → ready); we
	// mark handshake_complete once forwardUntilReady returns the first
	// ReadyForQuery byte. Byte counters update inline in the
	// command-phase loop.
	tracker := newConnTracker(p.onEvent, p.svcName, "postgres", "postgres",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()

	// Phase 1: Forward the startup handshake transparently.
	// Client sends startup message (no type byte, just length + payload).
	if err := p.forwardStartup(clientConn, serverConn); err != nil {
		closeReason = classifyCloseReason(err, "client")
		return
	}

	// Forward server's response to startup (AuthenticationOk, ParameterStatus, etc.)
	// until we see ReadyForQuery ('Z').
	if err := p.forwardUntilReady(serverConn, clientConn); err != nil {
		closeReason = classifyCloseReason(err, "server")
		return
	}
	tracker.EmitHandshakeComplete("", 0)

	// Phase 2: Proxy query messages.
	for {
		select {
		case <-ctx.Done():
			closeReason = "context_cancel"
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Read message type (1 byte) + length (4 bytes).
		header := make([]byte, 5)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		tracker.AddBytesC2S(5)

		msgType := header[0]
		msgLen := int(binary.BigEndian.Uint32(header[1:5]))
		if msgLen < 4 {
			closeReason = "io_error"
			return
		}

		// Read message body.
		body := make([]byte, msgLen-4)
		if len(body) > 0 {
			if _, err := io.ReadFull(clientConn, body); err != nil {
				closeReason = classifyCloseReason(err, "client")
				return
			}
			tracker.AddBytesC2S(len(body))
		}

		// Check if this is a Query ('Q') or Parse ('P') message.
		if msgType == 'Q' || msgType == 'P' {
			query := extractQuery(msgType, body)
			if query != "" {
				if handled := p.checkRules(clientConn, query); handled {
					continue
				}
			}
		}

		// Forward to server.
		if _, err := serverConn.Write(header); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		if len(body) > 0 {
			if _, err := serverConn.Write(body); err != nil {
				closeReason = classifyCloseReason(err, "server")
				return
			}
		}

		// Forward server response back to client until ReadyForQuery.
		// Server response bytes aren't counted at this level — the
		// helper doesn't expose its byte count and parsing it
		// inline would duplicate the logic. v1 of RFC-034 reports
		// client→server bytes only for postgres; mysql is similar.
		if err := p.forwardUntilReady(serverConn, clientConn); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
	}
}

// checkRules matches query against proxy rules. Returns true if handled.
func (p *postgresProxy) checkRules(clientConn net.Conn, query string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		// Query match uses SQL-aware canonicalization so rules keyed on
		// "SELECT * FROM users WHERE id = $1" match drivers' tight output
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
				errMsg = "ERROR: injected fault"
			}
			sendPgError(clientConn, errMsg)
			sendReadyForQuery(clientConn)

			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "postgres",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"query": query, "error": errMsg},
				})
			}
			return true

		case ActionDelay:
			// Delay applied above — don't intercept, let it forward.
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "postgres",
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

// forwardStartup forwards the initial startup message (no type byte).
func (p *postgresProxy) forwardStartup(client, server net.Conn) error {
	// Startup message: 4-byte length + payload (no type byte).
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(client, lenBuf); err != nil {
		return err
	}
	msgLen := int(binary.BigEndian.Uint32(lenBuf))
	if msgLen < 4 || msgLen > 10000 {
		return fmt.Errorf("invalid startup length: %d", msgLen)
	}

	body := make([]byte, msgLen-4)
	if _, err := io.ReadFull(client, body); err != nil {
		return err
	}

	// Forward to server.
	if _, err := server.Write(lenBuf); err != nil {
		return err
	}
	if _, err := server.Write(body); err != nil {
		return err
	}
	return nil
}

// forwardUntilReady copies server messages to client until ReadyForQuery ('Z').
// forwardUntilReady relays the startup exchange until the server reports
// ReadyForQuery ('Z').
//
// The exchange is BIDIRECTIONAL whenever the server asks for credentials, and
// this loop used to pump server→client only. Trust auth worked, because the
// server just sends AuthenticationOk and carries on. Every method that
// requires the client to answer — SCRAM-SHA-256 (the postgres:14+ default),
// MD5, cleartext — deadlocked: the client sent its 'p' response, the proxy
// never forwarded it, the server waited for credentials, the proxy waited for
// a 'Z' that could not arrive, and the client gave up at its 60-second read
// deadline with "connection reset by peer".
//
// The effect was that pg.sql.exec() could not talk to any password-protected
// Postgres — which is every realistic one — and reported a timeout rather than
// an authentication problem. It survived because no spec asserted on the
// result of a postgres step; they were all vacuous passes.
func (p *postgresProxy) forwardUntilReady(server, client net.Conn) error {
	for {
		msgType, body, err := readMessage(server)
		if err != nil {
			return err
		}
		if err := writeMessage(client, msgType, body); err != nil {
			return err
		}

		// An authentication request that expects an answer must have the
		// client's reply carried back, or both sides wait on each other.
		if msgType == 'R' && authNeedsClientReply(body) {
			replyType, replyBody, err := readMessage(client)
			if err != nil {
				return fmt.Errorf("read client auth response: %w", err)
			}
			if err := writeMessage(server, replyType, replyBody); err != nil {
				return fmt.Errorf("forward client auth response: %w", err)
			}
			continue
		}

		if msgType == 'Z' { // ReadyForQuery
			return nil
		}
	}
}

// authNeedsClientReply reports whether an Authentication message ('R')
// requires the client to send something back.
//
// Codes per the Postgres protocol: 0 AuthenticationOk, 3 CleartextPassword,
// 5 MD5Password, 10 SASL, 11 SASLContinue, 12 SASLFinal. The client answers 3,
// 5, 10 and 11; 0 and 12 are terminal and are followed by more server
// messages, so treating them as needing a reply would hang just as surely in
// the other direction.
func authNeedsClientReply(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	switch binary.BigEndian.Uint32(body[:4]) {
	case 3, 5, 10, 11:
		return true
	}
	return false
}

// readMessage reads one length-prefixed protocol message.
func readMessage(c net.Conn) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(c, header); err != nil {
		return 0, nil, err
	}
	msgLen := int(binary.BigEndian.Uint32(header[1:5]))
	if msgLen < 4 {
		return 0, nil, fmt.Errorf("invalid message length: %d", msgLen)
	}
	body := make([]byte, msgLen-4)
	if len(body) > 0 {
		if _, err := io.ReadFull(c, body); err != nil {
			return 0, nil, err
		}
	}
	return header[0], body, nil
}

// writeMessage writes one length-prefixed protocol message.
func writeMessage(c net.Conn, msgType byte, body []byte) error {
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(body)+4))
	if _, err := c.Write(header); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := c.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// extractQuery extracts the SQL string from a Query ('Q') or Parse ('P') message.
func extractQuery(msgType byte, body []byte) string {
	switch msgType {
	case 'Q':
		// Query message: null-terminated SQL string.
		if idx := strings.IndexByte(string(body), 0); idx >= 0 {
			return string(body[:idx])
		}
		return string(body)
	case 'P':
		// Parse message: statement name (null-terminated) + SQL (null-terminated).
		// Skip statement name.
		idx := 0
		for idx < len(body) && body[idx] != 0 {
			idx++
		}
		idx++ // skip null terminator
		// SQL starts here.
		start := idx
		for idx < len(body) && body[idx] != 0 {
			idx++
		}
		if start < len(body) {
			return string(body[start:idx])
		}
	}
	return ""
}

// sendPgError sends a Postgres ErrorResponse message.
func sendPgError(conn net.Conn, msg string) {
	// ErrorResponse: 'E' + length + severity + message + terminator
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, []byte("ERROR")...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator

	header := make([]byte, 5)
	header[0] = 'E'
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)+4))

	conn.Write(header)
	conn.Write(payload)
}

// sendReadyForQuery sends a ReadyForQuery message (idle state).
func sendReadyForQuery(conn net.Conn) {
	msg := []byte{'Z', 0, 0, 0, 5, 'I'} // type + length(5) + status(Idle)
	conn.Write(msg)
}

func (p *postgresProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *postgresProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *postgresProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	waitConns(&p.wg, p.onEvent, p.svcName, "postgres")
	return nil
}
