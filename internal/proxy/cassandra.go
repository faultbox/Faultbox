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
)

// Cassandra CQL binary protocol v4 framing.
//
// Frame header (9 bytes):
//   version (1) + flags (1) + stream_id (2) + opcode (1) + length (4)
//
// Relevant opcodes:
//   0x00 ERROR, 0x01 STARTUP, 0x02 READY, 0x03 AUTHENTICATE, 0x05 OPTIONS,
//   0x06 SUPPORTED, 0x07 QUERY, 0x08 RESULT, 0x09 PREPARE, 0x0A EXECUTE,
//   0x0B REGISTER, 0x0C EVENT, 0x0D BATCH, 0x0E AUTH_CHALLENGE,
//   0x0F AUTH_RESPONSE, 0x10 AUTH_SUCCESS
const (
	cqlOpError   byte = 0x00
	cqlOpQuery   byte = 0x07
	cqlOpPrepare byte = 0x09
	cqlOpExecute byte = 0x0A
	cqlOpBatch   byte = 0x0D

	// Response flag bit (v4+): response frames have the high bit set in
	// the version byte. Request frames from client do not.
	cqlResponseFlag byte = 0x80
)

type cassandraProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newCassandraProxy(onEvent OnProxyEvent, svcName string) *cassandraProxy {
	return &cassandraProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *cassandraProxy) Protocol() string { return "cassandra" }

func (p *cassandraProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *cassandraProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// Upstream → client (responses). Forwarded without inspection.
	go io.Copy(clientConn, serverConn)

	// Client → upstream (requests). Frame-aware to support fault injection
	// on QUERY / EXECUTE opcodes.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		header := make([]byte, 9)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			return
		}

		version := header[0]
		streamID := binary.BigEndian.Uint16(header[2:4])
		opcode := header[4]
		bodyLen := binary.BigEndian.Uint32(header[5:9])

		if bodyLen > 256*1024*1024 {
			return
		}

		body := make([]byte, bodyLen)
		if bodyLen > 0 {
			if _, err := io.ReadFull(clientConn, body); err != nil {
				return
			}
		}

		if opcode == cqlOpQuery || opcode == cqlOpExecute || opcode == cqlOpBatch {
			cql := extractCQL(opcode, body)
			if handled := p.checkRules(clientConn, version, streamID, cql); handled {
				continue
			}
		}

		// Forward to upstream.
		if _, err := serverConn.Write(header); err != nil {
			return
		}
		if bodyLen > 0 {
			if _, err := serverConn.Write(body); err != nil {
				return
			}
		}
	}
}

// extractCQL pulls the CQL statement text out of a request body. Returns
// empty string for opcodes where the CQL isn't directly embedded (EXECUTE
// references a prepared_id, BATCH contains multiple statements).
func extractCQL(opcode byte, body []byte) string {
	switch opcode {
	case cqlOpQuery:
		// QUERY body: [long string] CQL + consistency + flags + ...
		// long string = [int] length + bytes
		if len(body) < 4 {
			return ""
		}
		n := int(binary.BigEndian.Uint32(body[0:4]))
		if n < 0 || 4+n > len(body) {
			return ""
		}
		return string(body[4 : 4+n])
	case cqlOpExecute, cqlOpBatch:
		// EXECUTE references a prepared_id (MD5 hash), not the CQL text.
		// BATCH contains multiple statement-specs — matching against each
		// is more work than v1 needs. Return "" so rules can match by
		// opcode-wildcard ("*") or skip.
		return ""
	}
	return ""
}

func (p *cassandraProxy) checkRules(clientConn net.Conn, version byte, streamID uint16, cql string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if rule.Query != "" && !matchGlob(cql, rule.Query) {
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
				errMsg = "injected fault"
			}
			sendCassandraError(clientConn, version, streamID, errMsg)
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "cassandra",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"query": truncate(cql, 120), "error": errMsg},
				})
			}
			return true

		case ActionDrop:
			clientConn.Close()
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "cassandra",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"query": truncate(cql, 120), "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false
		}
	}
	return false
}

// sendCassandraError writes a CQL ERROR frame back to the client.
// Body: [int] error_code + [string] error_message.
// Using error code 0x0000 (ServerError) — drivers surface it as a generic
// server exception. Specific codes (UNAVAILABLE=0x1000, WRITE_TIMEOUT=0x1100)
// need additional body fields per the CQL v4 spec; future work.
func sendCassandraError(conn net.Conn, version byte, streamID uint16, msg string) {
	msgBytes := []byte(msg)
	body := make([]byte, 0, 4+2+len(msgBytes))

	// error_code = 0x0000 (ServerError)
	body = append(body, 0, 0, 0, 0)

	// [string] = short length + bytes
	body = append(body, 0, 0)
	binary.BigEndian.PutUint16(body[len(body)-2:], uint16(len(msgBytes)))
	body = append(body, msgBytes...)

	header := make([]byte, 9)
	header[0] = version | cqlResponseFlag
	header[1] = 0 // flags
	binary.BigEndian.PutUint16(header[2:4], streamID)
	header[4] = cqlOpError
	binary.BigEndian.PutUint32(header[5:9], uint32(len(body)))

	conn.Write(header)
	conn.Write(body)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

func (p *cassandraProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *cassandraProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *cassandraProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}
