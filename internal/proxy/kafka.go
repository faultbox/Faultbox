package proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"github.com/faultbox/Faultbox/internal/connowner"
	"github.com/faultbox/Faultbox/internal/kafkawire"
	"github.com/twmb/franz-go/pkg/kmsg"
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
	connections *connowner.Tracker
	mu          sync.RWMutex
	rules       []Rule
	topicIDs    map[[16]byte]string
	target      string
	listener    net.Listener
	onEvent     OnProxyEvent
	svcName     string
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	// RFC-038 Phase 3: TLS material. Kafka brokers expose
	// SSL/PLAINTEXT listeners on separate ports — there is no
	// in-band SSL upgrade like the postgres SSLRequest. So we
	// can wrap-and-dial directly: serverTLS wraps the listener,
	// clientTLS upgrades the upstream dial via proxy.Dial.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newKafkaProxy(onEvent OnProxyEvent, svcName string) *kafkaProxy {
	return &kafkaProxy{
		onEvent:  onEvent,
		topicIDs: make(map[[16]byte]string),
		svcName:  svcName,
	}
}

func (p *kafkaProxy) Protocol() string { return "kafka" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *kafkaProxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *kafkaProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	var ln net.Listener
	var listenAddr string
	var err error
	if p.serverTLS != nil {
		ln, listenAddr, err = ListenTLS(p.serverTLS)
	} else {
		ln, listenAddr, err = Listen()
	}
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

func (p *kafkaProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	// proxy.Dial routes through tls.Client + HandshakeContext when
	// clientTLS is set; otherwise it's net.DialTimeout. The 5s
	// budget covers both the TCP connect and the TLS handshake.
	serverConn, err := Dial(ctx, p.target, p.clientTLS, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()
	if p.connections != nil {
		release := p.connections.Forward(clientConn, serverConn)
		defer release()
	}
	// Shutdown must also release bindings for an idle connection.
	stop := context.AfterFunc(ctx, func() { clientConn.Close(); serverConn.Close() })
	defer stop()

	// RFC-034: per-connection lifecycle tracker. Kafka has no
	// explicit handshake — client jumps straight to Produce/Fetch
	// requests after TCP connect. We mark handshake_complete after
	// the first request-response round-trip as a stand-in for "this
	// connection is in steady-state command phase".
	tracker := newConnTracker(p.onEvent, p.svcName, "main", "kafka",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()
	requestsSeen := 0

	for {
		select {
		case <-ctx.Done():
			closeReason = "context_cancel"
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Kafka request: 4-byte length (big-endian) + payload.
		// Payload starts with: api_key(2) + api_version(2) + correlation_id(4) + client_id.
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		tracker.AddBytesC2S(4)
		msgLen := int(binary.BigEndian.Uint32(lenBuf))
		if msgLen <= 0 || msgLen > 10*1024*1024 {
			closeReason = "io_error"
			return
		}

		payload := make([]byte, msgLen)
		if _, err := io.ReadFull(clientConn, payload); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		tracker.AddBytesC2S(msgLen)

		// Parse API key to identify Produce/Fetch requests.
		duplicate := false
		if len(payload) >= 8 {
			apiKey := int16(binary.BigEndian.Uint16(payload[0:2]))
			topic := "" // Would need deeper parsing to extract topic

			if apiKey == kafkaAPIProduce || apiKey == kafkaAPIFetch {
				// Try to extract topic from the payload (simplified).
				topic = p.extractTopic(payload)

				handled, dup := p.checkRules(clientConn, apiKey, topic, payload)
				if handled {
					continue
				}
				duplicate = dup
			}
		}

		// Forward to server.
		if _, err := serverConn.Write(lenBuf); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		if _, err := serverConn.Write(payload); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}

		// acks=0 produces have no response; do not consume the next response.
		if req, _, _, err := kafkawire.DecodeRequest(payload); err == nil {
			if produce, ok := req.(*kmsg.ProduceRequest); ok && produce.Acks == 0 {
				if duplicate {
					serverConn.Write(lenBuf)
					serverConn.Write(payload)
				}
				continue
			}
		}
		// Forward response back.
		respLenBuf := make([]byte, 4)
		if _, err := io.ReadFull(serverConn, respLenBuf); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		tracker.AddBytesS2C(4)
		respLen := int(binary.BigEndian.Uint32(respLenBuf))
		if respLen <= 0 || respLen > 10*1024*1024 {
			closeReason = "io_error"
			return
		}
		resp := make([]byte, respLen)
		if _, err := io.ReadFull(serverConn, resp); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		tracker.AddBytesS2C(respLen)
		p.learnTopics(payload, resp)
		clientConn.Write(respLenBuf)
		clientConn.Write(resp)

		// #138: re-send the produce so the message lands on the broker a
		// second time (the client already got its single ack above). A
		// failed duplicate round-trip leaves serverConn mid-frame, so it
		// must kill the connection like any other framing error - limping
		// on would desync every later response.
		if duplicate {
			if err := p.forwardDuplicate(serverConn, lenBuf, payload, tracker); err != nil {
				closeReason = classifyCloseReason(err, "server")
				return
			}
		}

		requestsSeen++
		if requestsSeen == 1 {
			tracker.EmitHandshakeComplete("", 1)
		}
	}
}

// checkRules applies matching fault rules to a produce/fetch request.
// handled=true means the request was consumed (dropped or errored) and must
// not be forwarded. duplicate=true means forward normally AND once more, so
// the message lands on the broker twice (#138).
func (p *kafkaProxy) checkRules(clientConn net.Conn, apiKey int16, topic string, payload []byte) (handled bool, duplicate bool) {
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
			return true, false

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "kafka",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"api": apiName, "topic": topic, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false, false // forward after delay

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
			return true, false

		case ActionDuplicate:
			// Only a produce can be duplicated — duplicating a fetch is
			// meaningless. The rule simply does not apply to a fetch: keep
			// evaluating later rules instead of swallowing them.
			if apiKey != kafkaAPIProduce {
				continue
			}
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "kafka",
					Action:   "duplicate",
					To:       p.svcName,
					Fields:   map[string]string{"api": apiName, "topic": topic},
				})
			}
			return false, true
		}
	}
	return false, false
}

// forwardDuplicate re-sends a produce request to the broker so the message
// lands twice, then reads and discards the extra response (the client already
// received its single ack). Any failure is returned so the caller can kill
// the connection: a half-done round-trip leaves serverConn mid-frame, and
// silently continuing would desync every later response. The read carries a
// deadline because an acks=0 producer elicits no broker response at all —
// without it we would block and then steal the ack of the next request.
func (p *kafkaProxy) forwardDuplicate(serverConn net.Conn, lenBuf, payload []byte, tracker *connTracker) error {
	if _, err := serverConn.Write(lenBuf); err != nil {
		return err
	}
	if _, err := serverConn.Write(payload); err != nil {
		return err
	}
	tracker.AddBytesC2S(len(payload))

	serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer serverConn.SetReadDeadline(time.Time{})

	respLenBuf := make([]byte, 4)
	if _, err := io.ReadFull(serverConn, respLenBuf); err != nil {
		return err
	}
	tracker.AddBytesS2C(4)
	respLen := int(binary.BigEndian.Uint32(respLenBuf))
	if respLen <= 0 || respLen > 10*1024*1024 {
		return fmt.Errorf("duplicate response length %d out of range", respLen)
	}
	if _, err := io.CopyN(io.Discard, serverConn, int64(respLen)); err != nil {
		return err
	}
	tracker.AddBytesS2C(respLen)
	return nil
}

// extractTopic handles versioned request headers and flexible Kafka bodies.
func (p *kafkaProxy) extractTopic(payload []byte) string {
	req, _, _, err := kafkawire.DecodeRequest(payload)
	if err != nil {
		return ""
	}
	var name string
	var id [16]byte
	switch r := req.(type) {
	case *kmsg.ProduceRequest:
		if len(r.Topics) > 0 {
			name = r.Topics[0].Topic
			id = r.Topics[0].TopicID
		}
	case *kmsg.FetchRequest:
		if len(r.Topics) > 0 {
			name = r.Topics[0].Topic
			id = r.Topics[0].TopicID
		}
	}
	if name != "" {
		return name
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.topicIDs[id]
}

func (p *kafkaProxy) learnTopics(request, response []byte) {
	if len(request) < 2 || binary.BigEndian.Uint16(request) != 3 || len(response) < 4 {
		return
	}
	req, _, _, err := kafkawire.DecodeRequest(request)
	if err != nil {
		return
	}
	resp := req.ResponseKind()
	resp.SetVersion(req.GetVersion())
	body := response[4:]
	if resp.IsFlexible() {
		body, err = kafkawire.SkipTags(body)
		if err != nil {
			return
		}
	}
	if resp.ReadFrom(body) != nil {
		return
	}
	metadata := resp.(*kmsg.MetadataResponse)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, topic := range metadata.Topics {
		if topic.Topic != nil {
			p.topicIDs[topic.TopicID] = *topic.Topic
		}
	}
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
	waitConns(&p.wg, p.onEvent, p.svcName, "kafka")
	return nil
}
