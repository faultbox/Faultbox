package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

// udpProxy is a simple UDP relay with fault injection. It listens on a random
// UDP port, forwards each datagram to the target, and returns the response
// to the original sender (matched by source address).
//
// UDP is connectionless — there is no stream to hijack. "Drop" and "delay"
// operate per-datagram. corrupt() and reorder() from RFC-016 are not yet
// implemented; they need new Action variants and are tracked as open
// questions on that RFC.
type udpProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	conn     *net.UDPConn
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newUDPProxy(onEvent OnProxyEvent, svcName string) *udpProxy {
	return &udpProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *udpProxy) Protocol() string { return "udp" }

func (p *udpProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("resolve local: %w", err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.conn = conn
	ctx, p.cancel = context.WithCancel(ctx)

	targetAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("resolve target: %w", err)
	}

	p.wg.Add(1)
	go p.relay(ctx, targetAddr)

	return conn.LocalAddr().String(), nil
}

func (p *udpProxy) relay(ctx context.Context, targetAddr *net.UDPAddr) {
	defer p.wg.Done()

	// Per-client upstream connection, so responses route back to the right peer.
	// Keyed by client address string.
	upstreams := make(map[string]*net.UDPConn)
	var upstreamsMu sync.Mutex

	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		p.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, clientAddr, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}

		datagram := make([]byte, n)
		copy(datagram, buf[:n])

		if handled := p.applyRules(datagram, clientAddr); handled {
			continue
		}

		upstreamsMu.Lock()
		upConn, ok := upstreams[clientAddr.String()]
		if !ok {
			upConn, err = net.DialUDP("udp", nil, targetAddr)
			if err != nil {
				upstreamsMu.Unlock()
				continue
			}
			upstreams[clientAddr.String()] = upConn
			p.wg.Add(1)
			go p.responseLoop(ctx, upConn, clientAddr)
		}
		upstreamsMu.Unlock()

		upConn.Write(datagram)
	}
}

func (p *udpProxy) responseLoop(ctx context.Context, upConn *net.UDPConn, clientAddr *net.UDPAddr) {
	defer p.wg.Done()
	defer upConn.Close()

	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		upConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := upConn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		p.conn.WriteToUDP(buf[:n], clientAddr)
	}
}

func (p *udpProxy) applyRules(datagram []byte, clientAddr *net.UDPAddr) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		// UDP faults apply to all datagrams on the interface — no per-request
		// matching criteria beyond the target itself. Path/topic/etc. are
		// ignored; only probability filters the rule.
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
					Protocol: "udp",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"size": fmt.Sprintf("%d", len(datagram)), "from": clientAddr.String()},
				})
			}
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "udp",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			// Fall through — datagram still forwards, just delayed.
			return false
		}
	}
	return false
}

func (p *udpProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *udpProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *udpProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.conn != nil {
		p.conn.Close()
	}
	p.wg.Wait()
	return nil
}
