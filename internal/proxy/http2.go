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

	// RFC-038 Phase 3: TLS material. serverTLS wraps the listener
	// (ALPN h2 forced); clientTLS configures the http2.Transport
	// to dial upstream over TLS with ALPN h2.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newHTTP2Proxy(onEvent OnProxyEvent, svcName string) *http2Proxy {
	return &http2Proxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *http2Proxy) Protocol() string { return "http2" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *http2Proxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *http2Proxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	// Listen on random port. ListenTLS wraps the listener with ALPN
	// h2 negotiated via NextProtos. Plain Listen otherwise (h2c
	// upgrade happens at the handler level).
	var ln net.Listener
	var listenAddr string
	var err error
	if p.serverTLS != nil {
		serverCfg := p.serverTLS.Clone()
		serverCfg.NextProtos = []string{"h2"}
		ln, listenAddr, err = ListenTLS(serverCfg)
	} else {
		ln, listenAddr, err = Listen()
	}
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	// Upstream URL scheme: clientTLS non-nil ⇒ dial https:// with
	// ALPN h2; otherwise cleartext h2c http://.
	scheme := "http"
	if p.clientTLS != nil {
		scheme = "https"
	}
	targetURL, err := url.Parse(scheme + "://" + target)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("parse target URL: %w", err)
	}

	upstream := &http2.Transport{}
	if p.clientTLS != nil {
		clientCfg := p.clientTLS.Clone()
		if len(clientCfg.NextProtos) == 0 {
			clientCfg.NextProtos = []string{"h2"}
		}
		upstream.TLSClientConfig = clientCfg
	} else {
		// Cleartext upstream — h2c. Same shape as before RFC-038.
		upstream.AllowHTTP = true
		upstream.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)
	reverseProxy.Transport = upstream

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handleRequest(w, r, reverseProxy)
	})

	// RFC-034: connection-lifecycle events via http.Server.ConnState.
	// HTTP/2 multiplexes streams over a single TCP conn; we emit per
	// underlying-TCP-conn (StateNew/StateClosed), not per-stream.
	connTracker := NewHTTPConnStateTracker(p.onEvent, p.svcName, "main", "http2", target)

	// Listener side: when serverTLS is set, ALPN h2 is negotiated at
	// the TLS layer and http.Server.Serve dispatches via the http2
	// stack once http2.ConfigureServer runs. When serverTLS is nil
	// we keep the h2c upgrade path for prior-knowledge cleartext.
	var rootHandler http.Handler
	if p.serverTLS != nil {
		rootHandler = handler
	} else {
		rootHandler = h2c.NewHandler(handler, &http2.Server{})
	}

	p.server = &http.Server{
		Handler:   rootHandler,
		ConnState: connTracker.ConnState,
	}
	if p.serverTLS != nil {
		// Without ConfigureServer, http.Server.Serve falls back to
		// HTTP/1.1 even when ALPN negotiated h2. ConfigureServer
		// installs the http2 protocol handler on TLSNextProto.
		if err := http2.ConfigureServer(p.server, &http2.Server{}); err != nil {
			ln.Close()
			return "", fmt.Errorf("configure http2 server: %w", err)
		}
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
