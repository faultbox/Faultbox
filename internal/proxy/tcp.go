package proxy

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// tcpProxy is a raw, protocol-agnostic TCP relay with fault injection.
// Unlike the other proxies in this package it does not parse any wire
// format — bytes flow through io.Copy in both directions. Three rule
// actions are supported:
//
//   - ActionDrop    — close both sides immediately when rule matches.
//   - ActionDelay   — sleep Rule.Delay before relaying the next chunk.
//   - ActionRespond — write Rule.Body back to the client and close.
//
// Rule.Method is reused as a byte-prefix predicate on the first chunk
// from the client ("HELO" / "+OK" / etc.). An empty prefix matches any
// connection. For richer targeting use a protocol-specific proxy
// (http/grpc/postgres/...) instead.
//
// Shipped in v0.9.6 per RFC-024 follow-ups so plain-tcp services get
// the same data-path treatment as the 14 parsed protocols.
type tcpProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newTCPProxy(onEvent OnProxyEvent, svcName string) *tcpProxy {
	return &tcpProxy{onEvent: onEvent, svcName: svcName}
}

func (p *tcpProxy) Protocol() string { return "tcp" }

func (p *tcpProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.listener = ln
	ctx, p.cancel = context.WithCancel(ctx)

	p.wg.Add(1)
	go p.acceptLoop(ctx)

	return ln.Addr().String(), nil
}

func (p *tcpProxy) acceptLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if tcpLn, ok := p.listener.(*net.TCPListener); ok {
			_ = tcpLn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		}
		client, err := p.listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		p.wg.Add(1)
		go p.handle(ctx, client)
	}
}

// handle serves one client connection. It peeks the first chunk to feed
// byte-prefix rules, applies any matching action, and otherwise splices
// bytes bidirectionally to the upstream until either side closes.
func (p *tcpProxy) handle(ctx context.Context, client net.Conn) {
	defer p.wg.Done()
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	// Peek first chunk for prefix-based rule matching. 4 KiB is plenty
	// for protocol banners (HELO, SSH-2.0-, etc.) without pulling a
	// whole payload into memory.
	peek := make([]byte, 4096)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, readErr := client.Read(peek)
	_ = client.SetReadDeadline(time.Time{})
	first := peek[:n]

	if n > 0 {
		if handled := p.applyRules(ctx, first, client, upstream); handled {
			return
		}
		// Forward the peeked bytes to upstream before splicing.
		if _, werr := upstream.Write(first); werr != nil {
			return
		}
	}

	if readErr != nil && readErr != io.EOF {
		// Client closed or errored before sending anything; if there was
		// nothing to splice we're done.
		if n == 0 {
			return
		}
	}

	// Full-duplex splice.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// applyRules runs the rule list against the first client chunk. Returns
// true if a terminal action (drop / respond) fired — caller should close
// the connection instead of forwarding. Delay is non-terminal.
func (p *tcpProxy) applyRules(_ context.Context, first []byte, client, upstream net.Conn) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if !tcpPrefixMatches(first, rule.Method) {
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
			p.emit("drop", first, map[string]string{"size": fmt.Sprintf("%d", len(first))})
			_ = upstream.Close()
			return true
		case ActionRespond:
			if rule.Body != "" {
				_, _ = client.Write([]byte(rule.Body))
			}
			p.emit("respond", first, map[string]string{"body_size": fmt.Sprintf("%d", len(rule.Body))})
			return true
		case ActionDelay:
			p.emit("delay", first, map[string]string{"delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())})
			// Non-terminal: splice continues after sleep.
		}
	}
	return false
}

// tcpPrefixMatches reports whether first bytes begin with prefix, or
// whether prefix is empty (match-all). Callers that want more nuanced
// byte-pattern rules should use a protocol-specific proxy.
func tcpPrefixMatches(first []byte, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(string(first), prefix)
}

func (p *tcpProxy) emit(action string, first []byte, extra map[string]string) {
	if p.onEvent == nil {
		return
	}
	fields := map[string]string{"first_bytes": string(first[:min(16, len(first))])}
	for k, v := range extra {
		fields[k] = v
	}
	p.onEvent(ProxyEvent{
		Protocol: "tcp",
		Action:   action,
		To:       p.svcName,
		Fields:   fields,
	})
}

func (p *tcpProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *tcpProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *tcpProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}
