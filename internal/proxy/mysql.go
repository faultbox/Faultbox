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

// forwardHandshake handles the MySQL handshake phase.
func (p *mysqlProxy) forwardHandshake(client, server net.Conn) error {
	// Server sends greeting.
	if err := forwardMySQLPacket(server, client); err != nil {
		return fmt.Errorf("server greeting: %w", err)
	}
	// Client sends auth response.
	if err := forwardMySQLPacket(client, server); err != nil {
		return fmt.Errorf("client auth: %w", err)
	}
	// Server sends OK or auth switch.
	if err := forwardMySQLPacket(server, client); err != nil {
		return fmt.Errorf("server auth response: %w", err)
	}
	return nil
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
	header := make([]byte, 4)
	if _, err := io.ReadFull(src, header); err != nil {
		return err
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if _, err := dst.Write(header); err != nil {
		return err
	}
	if payloadLen > 0 {
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(src, payload); err != nil {
			return err
		}
		if _, err := dst.Write(payload); err != nil {
			return err
		}
	}
	return nil
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
