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
)

type httpProxy struct {
	mu      sync.RWMutex
	rules   []Rule
	target  string
	server  *http.Server
	onEvent OnProxyEvent
	svcName string

	// RFC-038 Phase 3: TLS material when the interface declares
	// tls=tls_cert(...). Either may be nil — see the TLSAware
	// docstring in proxy.go for the four combinations.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newHTTPProxy(onEvent OnProxyEvent, svcName string) *httpProxy {
	return &httpProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *httpProxy) Protocol() string { return "http" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *httpProxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *httpProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	// Listen on random port. ListenTLS wraps the listener via
	// tls.NewListener when serverTLS is set; otherwise plain Listen.
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

	// Upstream URL scheme follows the upstream-side TLS cfg: clientTLS
	// non-nil ⇒ the proxy dials the upstream over TLS, so the reverse
	// proxy must speak https://. Otherwise plain http://.
	scheme := "http"
	if p.clientTLS != nil {
		scheme = "https"
	}
	targetURL, err := url.Parse(scheme + "://" + target)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("parse target URL: %w", err)
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)

	// When dialing upstream over TLS, hand the resolved cfg to the
	// reverse proxy's transport so the customer's CA / mTLS material
	// applies. Otherwise the default transport (plain TCP) keeps
	// working unchanged.
	if p.clientTLS != nil {
		transport := &http.Transport{
			TLSClientConfig:       p.clientTLS,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		reverseProxy.Transport = transport
	}

	// RFC-034: connection-lifecycle events via http.Server.ConnState.
	// Per-conn open/close + first-request handshake_complete; byte
	// counts not yet wired (follow-up — needs Listener wrapper).
	connTracker := NewHTTPConnStateTracker(p.onEvent, p.svcName, "main", "http", target)

	p.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.handleRequest(w, r, reverseProxy)
		}),
		ConnState: connTracker.ConnState,
	}

	go func() {
		p.server.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		p.server.Close()
	}()

	return listenAddr, nil
}

func (p *httpProxy) handleRequest(w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) {
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

		// Probability check.
		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}

		// Apply delay first (for both ActionDelay and ActionError/ActionRespond).
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
					Protocol: "http",
					Action:   actionName(rule.Action),
					From:     "", // filled by runtime
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
			// Delay already applied above — forward normally.
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "http",
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
			// Close connection without response.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				if conn != nil {
					conn.Close()
				}
			}
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "http",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"method": method, "path": path},
				})
			}
			return
		}
	}

	// No matching rule (or ActionDelay fell through) — forward to real service.
	rp.ServeHTTP(w, r)
}

func (p *httpProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *httpProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *httpProxy) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}

func actionName(a Action) string {
	switch a {
	case ActionRespond:
		return "respond"
	case ActionError:
		return "error"
	case ActionDelay:
		return "delay"
	case ActionDrop:
		return "drop"
	case ActionDuplicate:
		return "duplicate"
	default:
		return "unknown"
	}
}

