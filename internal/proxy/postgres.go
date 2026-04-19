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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

	return ln.Addr().String(), nil
}

func (p *postgresProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// Phase 1: Forward the startup handshake transparently.
	// Client sends startup message (no type byte, just length + payload).
	if err := p.forwardStartup(clientConn, serverConn); err != nil {
		return
	}

	// Forward server's response to startup (AuthenticationOk, ParameterStatus, etc.)
	// until we see ReadyForQuery ('Z').
	if err := p.forwardUntilReady(serverConn, clientConn); err != nil {
		return
	}

	// Phase 2: Proxy query messages.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Read message type (1 byte) + length (4 bytes).
		header := make([]byte, 5)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			return
		}

		msgType := header[0]
		msgLen := int(binary.BigEndian.Uint32(header[1:5]))
		if msgLen < 4 {
			return
		}

		// Read message body.
		body := make([]byte, msgLen-4)
		if len(body) > 0 {
			if _, err := io.ReadFull(clientConn, body); err != nil {
				return
			}
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
			return
		}
		if len(body) > 0 {
			if _, err := serverConn.Write(body); err != nil {
				return
			}
		}

		// Forward server response back to client until ReadyForQuery.
		if err := p.forwardUntilReady(serverConn, clientConn); err != nil {
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
func (p *postgresProxy) forwardUntilReady(server, client net.Conn) error {
	for {
		header := make([]byte, 5)
		if _, err := io.ReadFull(server, header); err != nil {
			return err
		}

		msgType := header[0]
		msgLen := int(binary.BigEndian.Uint32(header[1:5]))
		if msgLen < 4 {
			return fmt.Errorf("invalid message length: %d", msgLen)
		}

		body := make([]byte, msgLen-4)
		if len(body) > 0 {
			if _, err := io.ReadFull(server, body); err != nil {
				return err
			}
		}

		// Forward to client.
		if _, err := client.Write(header); err != nil {
			return err
		}
		if len(body) > 0 {
			if _, err := client.Write(body); err != nil {
				return err
			}
		}

		if msgType == 'Z' { // ReadyForQuery
			return nil
		}
	}
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
	p.wg.Wait()
	return nil
}
