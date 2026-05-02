package proxy

import (
	"context"
	"crypto/tls"
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

	// RFC-038 Phase 3: TLS material. The generic TCP plugin handles
	// any "TLS from byte 1" service Faultbox doesn't have a dedicated
	// plugin for — same wrap-and-dial pattern as kafka / redis. The
	// prefix-peek rule predicate (Rule.Method) still fires against
	// the plaintext bytes between the two TLS legs.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newTCPProxy(onEvent OnProxyEvent, svcName string) *tcpProxy {
	return &tcpProxy{onEvent: onEvent, svcName: svcName}
}

func (p *tcpProxy) Protocol() string { return "tcp" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *tcpProxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *tcpProxy) Start(ctx context.Context, target string) (string, error) {
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

	// When the listener is TLS-wrapped (*tls.listener), the
	// SetDeadline-on-TCPListener trick in acceptLoop doesn't apply —
	// the type assertion fails silently. Close the listener on ctx
	// cancel so Accept returns and the loop exits cleanly without
	// relying on a Stop() call from the manager.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	p.wg.Add(1)
	go p.acceptLoop(ctx)

	return listenAddr, nil
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

	// proxy.Dial routes through tls.Client + HandshakeContext when
	// clientTLS is set; otherwise it's net.DialTimeout. The 5s
	// budget covers both the TCP connect and the TLS handshake.
	upstream, err := Dial(ctx, p.target, p.clientTLS, 5*time.Second)
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

	// RFC-034: per-connection lifecycle tracker. Mirrors the binary
	// proxy plugins — emits proxy_conn_open after dial, proxy_conn_close
	// in defer with byte counts + termination reason, proxy_stall when
	// either direction stops moving bytes for ≥ stallThreshold.
	tracker := newConnTracker(p.onEvent, p.svcName, "main", "tcp",
		client.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()
	// All TCP traffic is in command phase from the proxy's POV — no
	// handshake to mark complete. Set the bit so stall events render
	// with phase="command".
	tracker.handshakeDone.Store(true)

	// Full-duplex splice with byte counting + stall detection. Wrapped
	// readers on each side update the tracker's atomic counters inline;
	// a separate watchdog goroutine fires proxy_stall events when byte
	// progress halts.
	//
	// Exit semantics: select waits for the FIRST direction to terminate
	// or ctx.Done(). The other goroutine is allowed to leak briefly —
	// the deferred Close() on client + upstream causes its io.Copy to
	// return naturally a moment later. Don't wait for both `<-done` in
	// the select: a healthy long-lived connection (redis pipelining,
	// keepalives) leaves both io.Copy calls blocked forever, and
	// waiting on the second drain never returns until ctx cancel.
	//
	// done carries the closeReason from each direction. Routing the
	// reason through the channel rather than a shared variable avoids
	// the data race the -race builder flagged in v0.12.25 — both
	// io.Copy goroutines were writing closeReason concurrently, and
	// only the first one's value survives the select read anyway.
	done := make(chan string, 2)
	stallStop := make(chan struct{})

	go func() {
		clientReader := tracker.WrapClientReader(client)
		_, copyErr := io.Copy(upstream, clientReader)
		reason := "client_eof"
		if copyErr != nil && copyErr != io.EOF {
			reason = classifyCloseReason(copyErr, "client")
		}
		done <- reason
	}()
	go func() {
		serverReader := tracker.WrapServerReader(upstream)
		_, copyErr := io.Copy(client, serverReader)
		reason := "server_eof"
		if copyErr != nil && copyErr != io.EOF {
			reason = classifyCloseReason(copyErr, "server")
		}
		done <- reason
	}()
	go p.watchStalls(stallStop, tracker)

	select {
	case reason := <-done:
		closeReason = reason
	case <-ctx.Done():
		closeReason = "context_cancel"
	}
	close(stallStop)
}

// watchStalls polls the tracker's byte counters at 1Hz and emits
// proxy_stall when either direction has had no byte movement for
// stallThreshold (default 5s warn) or stallExtendThreshold (default
// 30s second event). One stall event per direction per tier per
// connection — RFC-034 §"Per-event volume control".
func (p *tcpProxy) watchStalls(stop <-chan struct{}, tracker *connTracker) {
	if tracker == nil || tracker.onEvent == nil {
		return
	}
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	var lastC2S, lastS2C int64
	var firstStallC2S, firstStallS2C time.Time
	emittedC2SWarn, emittedC2SExt := false, false
	emittedS2CWarn, emittedS2CExt := false, false

	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			curC2S := tracker.bytesC2S.Load()
			if curC2S != lastC2S {
				lastC2S = curC2S
				firstStallC2S = time.Time{}
				emittedC2SWarn, emittedC2SExt = false, false
			} else if curC2S > 0 {
				if firstStallC2S.IsZero() {
					firstStallC2S = now
				}
				stalled := now.Sub(firstStallC2S)
				if !emittedC2SWarn && stalled >= tracker.stallThreshold {
					tracker.EmitStall("client_to_server", "command", 0, stalled)
					emittedC2SWarn = true
				}
				if !emittedC2SExt && stalled >= tracker.stallExtendThreshold {
					tracker.EmitStall("client_to_server", "command", 0, stalled)
					emittedC2SExt = true
				}
			}

			curS2C := tracker.bytesS2C.Load()
			if curS2C != lastS2C {
				lastS2C = curS2C
				firstStallS2C = time.Time{}
				emittedS2CWarn, emittedS2CExt = false, false
			} else if curS2C > 0 {
				if firstStallS2C.IsZero() {
					firstStallS2C = now
				}
				stalled := now.Sub(firstStallS2C)
				if !emittedS2CWarn && stalled >= tracker.stallThreshold {
					tracker.EmitStall("server_to_client", "command", 0, stalled)
					emittedS2CWarn = true
				}
				if !emittedS2CExt && stalled >= tracker.stallExtendThreshold {
					tracker.EmitStall("server_to_client", "command", 0, stalled)
					emittedS2CExt = true
				}
			}
		}
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
