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
)

// AMQP 0-9-1 frame types and method IDs.
const (
	amqpFrameMethod    = 1
	amqpFrameHeader    = 2
	amqpFrameBody      = 3
	amqpFrameHeartbeat = 8

	// Basic class (60) methods.
	amqpClassBasic    = 60
	amqpMethodPublish = 40
	amqpMethodDeliver = 60
)

type amqpProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newAMQPProxy(onEvent OnProxyEvent, svcName string) *amqpProxy {
	return &amqpProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *amqpProxy) Protocol() string { return "amqp" }

func (p *amqpProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *amqpProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// RFC-034: per-connection lifecycle tracker. AMQP has a protocol
	// header "AMQP\x00\x00\x09\x01" then bidirectional frame flow;
	// the header serves as the handshake boundary marker. Byte
	// counters update inside forwardFrames per direction.
	tracker := newConnTracker(p.onEvent, p.svcName, "main", "amqp",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()

	// AMQP starts with protocol header: "AMQP\x00\x00\x09\x01"
	// Forward the handshake bidirectionally until both sides are ready.
	// Simplified: forward the protocol header from client to server.
	protoHeader := make([]byte, 8)
	if _, err := io.ReadFull(clientConn, protoHeader); err != nil {
		closeReason = classifyCloseReason(err, "client")
		return
	}
	tracker.AddBytesC2S(8)
	if _, err := serverConn.Write(protoHeader); err != nil {
		closeReason = classifyCloseReason(err, "server")
		return
	}
	tracker.EmitHandshakeComplete("amqp_proto_header", 1)

	// Bidirectional frame forwarding with interception. Wait for
	// first errCh only — second goroutine drains naturally when
	// deferred conn closes propagate (same lesson as tcp.go's
	// single-`<-done` semantic).
	errCh := make(chan error, 2)

	// Server → client (delivery path).
	go func() {
		errCh <- p.forwardFrames(ctx, serverConn, clientConn, "deliver", tracker.AddBytesS2C)
	}()

	// Client → server (publish path).
	go func() {
		errCh <- p.forwardFrames(ctx, clientConn, serverConn, "publish", tracker.AddBytesC2S)
	}()

	if err := <-errCh; err != nil {
		closeReason = classifyCloseReason(err, "client")
	}
}

// forwardFrames reads AMQP frames from src and writes to dst.
// Intercepts Basic.Publish frames for fault injection.
//
// addBytes records bytes read from `src` so the connection-lifecycle
// tracker can report bytes_c2s / bytes_s2c on proxy_conn_close. nil
// is fine — counter call is conditional.
func (p *amqpProxy) forwardFrames(ctx context.Context, src, dst net.Conn, direction string, addBytes func(int)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		src.SetReadDeadline(time.Now().Add(60 * time.Second))

		// AMQP frame: type(1) + channel(2) + size(4) + payload + frame-end(1)
		frameHeader := make([]byte, 7)
		if _, err := io.ReadFull(src, frameHeader); err != nil {
			return err
		}
		if addBytes != nil {
			addBytes(7)
		}

		frameType := frameHeader[0]
		frameSize := int(binary.BigEndian.Uint32(frameHeader[3:7]))

		payload := make([]byte, frameSize+1) // +1 for frame-end byte (0xCE)
		if _, err := io.ReadFull(src, payload); err != nil {
			return err
		}
		if addBytes != nil {
			addBytes(len(payload))
		}

		// Check for Basic.Publish method frame.
		if frameType == amqpFrameMethod && frameSize >= 4 {
			classID := binary.BigEndian.Uint16(payload[0:2])
			methodID := binary.BigEndian.Uint16(payload[2:4])

			if classID == amqpClassBasic && methodID == amqpMethodPublish {
				exchange, routingKey := parseBasicPublish(payload[4:])

				if handled := p.checkRules(src, direction, exchange, routingKey); handled {
					continue // drop this frame
				}
			}
		}

		// Forward frame.
		if _, err := dst.Write(frameHeader); err != nil {
			return err
		}
		if _, err := dst.Write(payload); err != nil {
			return err
		}
	}
}

func (p *amqpProxy) checkRules(conn net.Conn, direction, exchange, routingKey string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	// Match topic against exchange or routing key.
	topic := routingKey
	if topic == "" {
		topic = exchange
	}

	for _, rule := range rules {
		if rule.Topic != "" && !matchGlob(topic, rule.Topic) {
			continue
		}
		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}

		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}

		switch rule.Action {
		case ActionDrop:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "amqp",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"exchange": exchange, "routing_key": routingKey, "direction": direction},
				})
			}
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "amqp",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"exchange": exchange, "routing_key": routingKey, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false

		case ActionError:
			conn.Close()
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "amqp",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"exchange": exchange, "routing_key": routingKey, "error": rule.Error},
				})
			}
			return true
		}
	}
	return false
}

// parseBasicPublish extracts exchange and routing key from Basic.Publish payload.
// Format: reserved(2) + exchange(shortstr) + routing_key(shortstr) + mandatory(1) + immediate(1)
func parseBasicPublish(data []byte) (exchange, routingKey string) {
	if len(data) < 3 {
		return "", ""
	}
	offset := 2 // skip reserved short

	// Short string: length(1) + bytes.
	if offset >= len(data) {
		return "", ""
	}
	exLen := int(data[offset])
	offset++
	if offset+exLen > len(data) {
		return "", ""
	}
	exchange = string(data[offset : offset+exLen])
	offset += exLen

	if offset >= len(data) {
		return exchange, ""
	}
	rkLen := int(data[offset])
	offset++
	if offset+rkLen > len(data) {
		return exchange, ""
	}
	routingKey = string(data[offset : offset+rkLen])

	return exchange, routingKey
}

func (p *amqpProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *amqpProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *amqpProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}
