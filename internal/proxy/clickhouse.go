package proxy

import (
	"bytes"
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

// clickhouseProxy reuses the HTTP proxy machinery but adds SQL-body
// matching. ClickHouse drivers POST SQL as the HTTP request body (the
// default) or URL-encode it as ?query=... — we inspect both and match
// against rule.Query.
type clickhouseProxy struct {
	mu      sync.RWMutex
	rules   []Rule
	target  string
	server  *http.Server
	onEvent OnProxyEvent
	svcName string
}

func newClickhouseProxy(onEvent OnProxyEvent, svcName string) *clickhouseProxy {
	return &clickhouseProxy{onEvent: onEvent, svcName: svcName}
}

func (p *clickhouseProxy) Protocol() string { return "clickhouse" }

func (p *clickhouseProxy) Start(ctx context.Context, target string) (string, error) {
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
	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)

	p.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.handle(w, r, reverseProxy)
		}),
	}
	go p.server.Serve(ln)
	go func() {
		<-ctx.Done()
		p.server.Close()
	}()
	return ln.Addr().String(), nil
}

func (p *clickhouseProxy) handle(w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) {
	sql := extractClickhouseSQL(r)

	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if rule.Query != "" && !matchGlob(sql, rule.Query) {
			continue
		}
		if rule.Prob > 0 && rand.Float64() > rule.Prob {
			continue
		}
		if rule.Delay > 0 {
			time.Sleep(rule.Delay)
		}

		switch rule.Action {
		case ActionError, ActionRespond:
			status := rule.Status
			if status == 0 {
				// ClickHouse error responses are HTTP 500 with a plain text body.
				// Drivers parse the body for the exception text.
				status = http.StatusInternalServerError
			}
			msg := rule.Error
			if msg == "" {
				msg = "injected fault"
			}
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(status)
			io.WriteString(w, "Code: 0. DB::Exception: "+msg)
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "clickhouse",
					Action:   actionName(rule.Action),
					To:       p.svcName,
					Fields:   map[string]string{"query": truncateSQL(sql, 120), "status": fmt.Sprintf("%d", status)},
				})
			}
			return

		case ActionDrop:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, _ := hj.Hijack(); conn != nil {
					conn.Close()
				}
			}
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "clickhouse",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"query": truncateSQL(sql, 120)},
				})
			}
			return

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "clickhouse",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"query": truncateSQL(sql, 120), "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
		}
	}

	rp.ServeHTTP(w, r)
}

// extractClickhouseSQL pulls SQL text from either the POST body or the
// ?query=... URL parameter. The request body is read into memory and
// replaced so the reverse proxy can forward it unchanged.
func extractClickhouseSQL(r *http.Request) string {
	if q := r.URL.Query().Get("query"); q != "" {
		return q
	}
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	r.Body.Close()
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return string(body)
}

func truncateSQL(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (p *clickhouseProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *clickhouseProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *clickhouseProxy) Stop() error {
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}
