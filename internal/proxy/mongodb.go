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

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MongoDB wire protocol opcodes.
const (
	opMsgCode = 2013 // OP_MSG (modern, MongoDB 3.6+)
)

type mongodbProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newMongoDBProxy(onEvent OnProxyEvent, svcName string) *mongodbProxy {
	return &mongodbProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *mongodbProxy) Protocol() string { return "mongodb" }

func (p *mongodbProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *mongodbProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// MongoDB message header: length(4) + requestID(4) + responseTo(4) + opCode(4)
		header := make([]byte, 16)
		if _, err := io.ReadFull(clientConn, header); err != nil {
			return
		}

		msgLen := int(binary.LittleEndian.Uint32(header[0:4]))
		requestID := binary.LittleEndian.Uint32(header[4:8])
		opCode := int32(binary.LittleEndian.Uint32(header[12:16]))

		if msgLen < 16 || msgLen > 48*1024*1024 {
			return
		}

		body := make([]byte, msgLen-16)
		if len(body) > 0 {
			if _, err := io.ReadFull(clientConn, body); err != nil {
				return
			}
		}

		// Only intercept OP_MSG (modern protocol).
		if opCode == opMsgCode {
			cmd, collection := parseOPMSG(body)
			if cmd != "" {
				if handled := p.checkRules(clientConn, requestID, cmd, collection); handled {
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

		// Forward response back.
		respHeader := make([]byte, 16)
		if _, err := io.ReadFull(serverConn, respHeader); err != nil {
			return
		}
		respLen := int(binary.LittleEndian.Uint32(respHeader[0:4]))
		respBody := make([]byte, respLen-16)
		if len(respBody) > 0 {
			if _, err := io.ReadFull(serverConn, respBody); err != nil {
				return
			}
		}
		clientConn.Write(respHeader)
		clientConn.Write(respBody)
	}
}

func (p *mongodbProxy) checkRules(clientConn net.Conn, requestID uint32, cmd, collection string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		// Match command against Method field, collection against Key field.
		if rule.Method != "" && !matchGlob(cmd, rule.Method) {
			continue
		}
		if rule.Key != "" && !matchGlob(collection, rule.Key) {
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
			sendMongoError(clientConn, requestID, errMsg)
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "mongodb",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"command": cmd, "collection": collection, "error": errMsg},
				})
			}
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "mongodb",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"command": cmd, "collection": collection, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
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

// parseOPMSG extracts the command name and collection from an OP_MSG body.
// OP_MSG: flagBits(4) + sections. Section kind 0: BSON document.
func parseOPMSG(body []byte) (cmd string, collection string) {
	if len(body) < 5 {
		return "", ""
	}
	// Skip flagBits (4 bytes).
	offset := 4

	// Section kind 0: single BSON document.
	if body[offset] != 0 {
		return "", ""
	}
	offset++

	// Parse BSON document — extract first key (command name) and its value.
	if offset+4 > len(body) {
		return "", ""
	}
	bsonLen := int(binary.LittleEndian.Uint32(body[offset:]))
	if offset+bsonLen > len(body) {
		return "", ""
	}
	bsonDoc := body[offset : offset+bsonLen]

	// Simple BSON parsing: first element is the command.
	// BSON: total_len(4) + elements + \0
	pos := 4
	if pos >= len(bsonDoc) {
		return "", ""
	}
	elemType := bsonDoc[pos]
	pos++

	// Read key name (null-terminated).
	keyStart := pos
	for pos < len(bsonDoc) && bsonDoc[pos] != 0 {
		pos++
	}
	if pos >= len(bsonDoc) {
		return "", ""
	}
	cmd = string(bsonDoc[keyStart:pos])
	pos++ // skip null

	// Read value based on type.
	switch elemType {
	case 0x02: // string — the collection name for commands like find, insert, etc.
		if pos+4 > len(bsonDoc) {
			return cmd, ""
		}
		strLen := int(binary.LittleEndian.Uint32(bsonDoc[pos:]))
		pos += 4
		if pos+strLen > len(bsonDoc) {
			return cmd, ""
		}
		collection = string(bsonDoc[pos : pos+strLen-1]) // -1 for null terminator
	}

	return cmd, collection
}

// sendMongoError sends a MongoDB OP_MSG error response. The body is encoded
// as real BSON so official drivers recognize it as a proper server error
// (ok=0, errmsg=..., code=...). Sending JSON here would leave drivers in an
// undefined state — they would either hang waiting for more bytes or abort
// the connection, making fault outcomes indistinguishable from proxy bugs.
func sendMongoError(conn net.Conn, requestID uint32, errMsg string) {
	bsonData, err := bson.Marshal(bson.M{
		"ok":     0.0,
		"errmsg": errMsg,
		"code":   1,
	})
	if err != nil {
		return
	}

	// OP_MSG body: flagBits(4) + section kind 0 + BSON document.
	msgBody := make([]byte, 0, 4+1+len(bsonData))
	msgBody = append(msgBody, 0, 0, 0, 0) // flagBits = 0
	msgBody = append(msgBody, 0)          // section kind 0 (single body document)
	msgBody = append(msgBody, bsonData...)

	// Header: length(4) + requestID(4) + responseTo(4) + opCode(4)
	totalLen := 16 + len(msgBody)
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], uint32(totalLen))
	binary.LittleEndian.PutUint32(header[4:8], requestID+1)
	binary.LittleEndian.PutUint32(header[8:12], requestID)
	binary.LittleEndian.PutUint32(header[12:16], uint32(opMsgCode))

	conn.Write(header)
	conn.Write(msgBody)
}

func (p *mongodbProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *mongodbProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *mongodbProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}
