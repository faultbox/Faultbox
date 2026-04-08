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

// Kafka API keys for the requests we care about.
const (
	kafkaAPIProduce = 0
	kafkaAPIFetch   = 1
)

type kafkaProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newKafkaProxy(onEvent OnProxyEvent, svcName string) *kafkaProxy {
	return &kafkaProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *kafkaProxy) Protocol() string { return "kafka" }

func (p *kafkaProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *kafkaProxy) handleConn(ctx context.Context, clientConn net.Conn) {
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

		// Kafka request: 4-byte length (big-endian) + payload.
		// Payload starts with: api_key(2) + api_version(2) + correlation_id(4) + client_id.
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
			return
		}
		msgLen := int(binary.BigEndian.Uint32(lenBuf))
		if msgLen <= 0 || msgLen > 10*1024*1024 {
			return
		}

		payload := make([]byte, msgLen)
		if _, err := io.ReadFull(clientConn, payload); err != nil {
			return
		}

		// Parse API key to identify Produce/Fetch requests.
		if len(payload) >= 8 {
			apiKey := int16(binary.BigEndian.Uint16(payload[0:2]))
			topic := "" // Would need deeper parsing to extract topic

			if apiKey == kafkaAPIProduce || apiKey == kafkaAPIFetch {
				// Try to extract topic from the payload (simplified).
				topic = p.extractTopic(payload)

				if handled := p.checkRules(clientConn, apiKey, topic, payload); handled {
					continue
				}
			}
		}

		// Forward to server.
		if _, err := serverConn.Write(lenBuf); err != nil {
			return
		}
		if _, err := serverConn.Write(payload); err != nil {
			return
		}

		// Forward response back.
		respLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(serverConn, respLenBuf); err != nil {
			return
		}
		respLen := int(binary.BigEndian.Uint32(respLenBuf))
		if respLen <= 0 || respLen > 10*1024*1024 {
			return
		}
		resp := make([]byte, respLen)
		if _, err := io.ReadFull(serverConn, resp); err != nil {
			return
		}
		clientConn.Write(respLenBuf)
		clientConn.Write(resp)
	}
}

func (p *kafkaProxy) checkRules(clientConn net.Conn, apiKey int16, topic string, payload []byte) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

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

		apiName := "produce"
		if apiKey == kafkaAPIFetch {
			apiName = "fetch"
		}

		switch rule.Action {
		case ActionDrop:
			// Don't forward, don't respond — message is lost.
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "kafka",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"api": apiName, "topic": topic},
				})
			}
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "kafka",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"api": apiName, "topic": topic, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false // forward after delay

		case ActionError:
			// Close connection to simulate broker error.
			clientConn.Close()
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "kafka",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"api": apiName, "topic": topic, "error": rule.Error},
				})
			}
			return true
		}
	}
	return false
}

// extractTopic tries to extract the topic name from a Kafka Produce/Fetch request.
// This is simplified — real parsing requires knowing the API version.
func (p *kafkaProxy) extractTopic(payload []byte) string {
	// Skip: api_key(2) + api_version(2) + correlation_id(4)
	offset := 8

	// Skip client_id (2-byte length + string).
	if offset+2 > len(payload) {
		return ""
	}
	clientIDLen := int(int16(binary.BigEndian.Uint16(payload[offset:])))
	offset += 2
	if clientIDLen > 0 {
		offset += clientIDLen
	}

	// For Produce (API v0-2): skip acks(2) + timeout(4) + topic_count(4).
	// Then: topic_name_len(2) + topic_name.
	// This is highly version-dependent — simplified extraction.
	// Scan forward looking for a reasonable-length string.
	for i := offset; i < len(payload)-2 && i < offset+50; i++ {
		strLen := int(int16(binary.BigEndian.Uint16(payload[i:])))
		if strLen > 0 && strLen < 256 && i+2+strLen <= len(payload) {
			candidate := string(payload[i+2 : i+2+strLen])
			// Heuristic: topic names are alphanumeric with dots/dashes/underscores.
			if isTopicName(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func isTopicName(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (p *kafkaProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *kafkaProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *kafkaProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}
