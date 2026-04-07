package proxy

import (
	"context"
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
}

func newHTTPProxy(onEvent OnProxyEvent, svcName string) *httpProxy {
	return &httpProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *httpProxy) Protocol() string { return "http" }

func (p *httpProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	// Listen on random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	targetURL, err := url.Parse("http://" + target)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("parse target URL: %w", err)
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)

	p.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.handleRequest(w, r, reverseProxy)
		}),
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

