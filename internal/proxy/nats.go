package proxy

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"net"
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

	// Server → client.
	go func() {
		scanner := bufio.NewScanner(serverConn)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		linesSeen := 0
		for scanner.Scan() {
			line := scanner.Text()
			tracker.AddBytesS2C(len(line) + 1) // +1 for newline
			// Intercept MSG lines (delivery).
			if strings.HasPrefix(line, "MSG ") {
				subject := extractNATSSubject(line)
				if p.shouldDrop(subject, "deliver") {
					continue
				}
			}
			fmt.Fprintln(clientConn, line)
			linesSeen++
			if linesSeen == 1 {
				// First server line (typically `INFO {...}`) marks
				// the connection-ready state.
				tracker.EmitHandshakeComplete("info", 1)
			}
		}
		errCh <- scanner.Err()
	}()

	// Client → server.
	go func() {
		scanner := bufio.NewScanner(clientConn)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			tracker.AddBytesC2S(len(line) + 1)
			// Intercept PUB lines (publish).
			if strings.HasPrefix(line, "PUB ") {
				subject := extractNATSSubject(line)
				if p.shouldDrop(subject, "publish") {
					continue
				}
			}
			fmt.Fprintln(serverConn, line)
		}
		errCh <- scanner.Err()
	}()

	if err := <-errCh; err != nil {
		closeReason = classifyCloseReason(err, "client")
	}
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

func (p *natsProxy) AddRule(rule Rule)  { p.mu.Lock(); p.rules = append(p.rules, rule); p.mu.Unlock() }
func (p *natsProxy) ClearRules()        { p.mu.Lock(); p.rules = nil; p.mu.Unlock() }
func (p *natsProxy) Stop() error {
	if p.cancel != nil { p.cancel() }
	if p.listener != nil { p.listener.Close() }
	p.wg.Wait()
	return nil
}
