package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type http2Proxy struct {
	mu      sync.RWMutex
	rules   []Rule
	target  string
	server  *http.Server
	onEvent OnProxyEvent
	svcName string
}

func newHTTP2Proxy(onEvent OnProxyEvent, svcName string) *http2Proxy {
	return &http2Proxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *http2Proxy) Protocol() string { return "http2" }

func (p *http2Proxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	targetURL, err := url.Parse("http://" + target)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("parse target URL: %w", err)
	}

	// Upstream transport speaks HTTP/2 over cleartext — matches the most
	// common service-mesh deployment. For TLS upstream, the transport would
	// need its TLS config wired from the container trust store; out of scope
	// for v1.
	upstream := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)
	reverseProxy.Transport = upstream

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handleRequest(w, r, reverseProxy)
	})

	// h2c.NewHandler upgrades prior-knowledge HTTP/2 cleartext connections
	// in the inbound direction. Clients that send HTTP/1.1 still work.
	p.server = &http.Server{
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}

	go func() {
		p.server.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		p.server.Close()
	}()

	return ln.Addr().String(), nil
}

func (p *http2Proxy) handleRequest(w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	method := r.Method
	path := r.URL.Path

	for _, rule := range rules {
		if !rule.MatchRequest(method, path, "", "", "", "") {
			continue
		}

		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}

		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}

		switch rule.Action {
		case ActionRespond, ActionError:
			status := rule.Status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			body := rule.Body
			if body == "" && rule.Error != "" {
				body = fmt.Sprintf(`{"error":%q}`, rule.Error)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, body)

			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "http2",
					Action:   actionName(rule.Action),
					To:       p.svcName,
					Fields: map[string]string{
						"method": method,
						"path":   path,
						"status": fmt.Sprintf("%d", status),
					},
				})
			}
			return

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "http2",
					Action:   "delay",
					To:       p.svcName,
					Fields: map[string]string{
						"method":   method,
						"path":     path,
						"delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds()),
					},
				})
			}

		case ActionDrop:
			// HTTP/2 streams can't be "hijacked" like HTTP/1.1 — the closest
			// equivalent is RST_STREAM via a panic-captured response, which
			// Go's server translates to an internal reset. We use status 499
			// (nginx's "client closed connection") and close without writing
			// a body to trigger stream-level errors on the client.
			w.WriteHeader(499)
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "http2",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"method": method, "path": path},
				})
			}
			return
		}
	}

	rp.ServeHTTP(w, r)
}

func (p *http2Proxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *http2Proxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *http2Proxy) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}
