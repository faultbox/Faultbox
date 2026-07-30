package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type natsProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newNATSProxy(onEvent OnProxyEvent, svcName string) *natsProxy {
	return &natsProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *natsProxy) Protocol() string { return "nats" }

func (p *natsProxy) Start(ctx context.Context, target string) (string, error) {
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

func (p *natsProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// RFC-034: per-connection lifecycle tracker. NATS sends an
	// INFO line immediately after connect (the handshake-equivalent
	// — server announces itself); we mark handshake_complete after
	// the first server-line reaches the client.
	tracker := newConnTracker(p.onEvent, p.svcName, "main", "nats",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()

	errCh := make(chan error, 2)

	// Server → client: MSG carries a payload, everything else is a bare line.
	go func() {
		linesSeen := 0
		errCh <- p.relayNATS(serverConn, clientConn, "MSG ", "deliver",
			tracker.AddBytesS2C, func() {
				linesSeen++
				if linesSeen == 1 {
					// First server line (typically `INFO {...}`) marks the
					// connection-ready state.
					tracker.EmitHandshakeComplete("info", 1)
				}
			})
	}()

	// Client → server: PUB carries a payload.
	go func() {
		errCh <- p.relayNATS(clientConn, serverConn, "PUB ", "publish",
			tracker.AddBytesC2S, func() {})
	}()

	if err := <-errCh; err != nil {
		closeReason = classifyCloseReason(err, "client")
	}
}

// relayNATS forwards the NATS protocol from src to dst, dropping messages whose
// subject matches a rule.
//
// # Why this is byte-oriented
//
// This used to use a bufio.Scanner and fmt.Fprintln. Both are wrong for NATS:
//
//   - NATS frames on CRLF. Scanner's ScanLines strips the trailing "\r" and
//     Fprintln writes back only "\n", so **every line the proxy touched lost a
//     byte**. The Go client's parser reported the memorable
//     `nats: expected 'PONG', got 'PONG'` — same text, different framing.
//   - PUB and MSG carry a length-prefixed, possibly binary payload. Splitting
//     that on newlines corrupts any payload containing one, and lets payload
//     bytes be mistaken for a protocol verb.
//
// So control lines are forwarded with their terminator intact, and a payload is
// read by its declared length and forwarded opaquely. Dropping a message drops
// its payload with it, which the line-based version could not do at all.
func (p *natsProxy) relayNATS(src, dst net.Conn, verb, direction string, count func(int), onLine func()) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	for {
		// ReadBytes keeps the delimiter, so "\r\n" survives intact.
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return err
		}
		count(len(line))

		var payload []byte
		if n := natsPayloadLen(line, verb); n >= 0 {
			// n bytes of payload plus its own CRLF terminator.
			payload = make([]byte, n+2)
			if _, rerr := io.ReadFull(reader, payload); rerr != nil {
				return rerr
			}
			count(len(payload))
		}

		drop := false
		if bytes.HasPrefix(line, []byte(verb)) {
			if p.shouldDrop(extractNATSSubject(string(line)), direction) {
				drop = true
			}
		}

		if !drop {
			if _, werr := dst.Write(line); werr != nil {
				return werr
			}
			if len(payload) > 0 {
				if _, werr := dst.Write(payload); werr != nil {
					return werr
				}
			}
			onLine()
		}

		if err != nil {
			return err
		}
	}
}

// natsPayloadLen returns the declared payload size of a PUB/MSG control line,
// or -1 when the line carries no payload.
//
// Both verbs put the byte count last:
//
//	PUB <subject> [reply-to] <#bytes>\r\n<payload>\r\n
//	MSG <subject> <sid> [reply-to] <#bytes>\r\n<payload>\r\n
//
// HPUB/HMSG (headers) declare two sizes, of which the last is the total — so
// reading the final field is correct for those too.
func natsPayloadLen(line []byte, verb string) int {
	if !bytes.HasPrefix(line, []byte(verb)) {
		return -1
	}
	fields := strings.Fields(strings.TrimRight(string(line), "\r\n"))
	if len(fields) < 3 {
		return -1
	}
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func (p *natsProxy) shouldDrop(subject, direction string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if rule.Topic != "" && !matchGlob(subject, rule.Topic) {
			continue
		}
		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}

		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}

		if rule.Action == ActionDrop {
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "nats",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"subject": subject, "direction": direction},
				})
			}
			return true
		}
		if rule.Action == ActionDelay {
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "nats",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"subject": subject, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
		}
	}
	return false
}

// extractNATSSubject gets the subject from PUB/MSG lines.
// PUB subject [reply-to] #bytes
// MSG subject sid [reply-to] #bytes
func extractNATSSubject(line string) string {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func (p *natsProxy) AddRule(rule Rule) { p.mu.Lock(); p.rules = append(p.rules, rule); p.mu.Unlock() }
func (p *natsProxy) ClearRules()       { p.mu.Lock(); p.rules = nil; p.mu.Unlock() }
func (p *natsProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	waitConns(&p.wg, p.onEvent, p.svcName, "nats")
	return nil
}
