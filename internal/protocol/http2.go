package protocol

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func init() {
	Register(&http2Protocol{})
}

type http2Protocol struct{}

func (p *http2Protocol) Name() string { return "http2" }

func (p *http2Protocol) Methods() []string {
	return []string{"get", "post", "put", "delete", "patch"}
}

func (p *http2Protocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := newH2CClient(2 * time.Second)
	url := normalizeHTTPURL(addr)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("http2 healthcheck %q timed out after %s", addr, timeout)
}

func (p *http2Protocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	httpMethod := strings.ToUpper(method)
	switch httpMethod {
	case "GET", "POST", "PUT", "DELETE", "PATCH":
	default:
		return nil, fmt.Errorf("unsupported HTTP/2 method %q", method)
	}

	path := getStringKwarg(kwargs, "path", "/")
	url := fmt.Sprintf("%s%s", normalizeHTTPURL(addr), path)

	var bodyReader io.Reader
	if body := getStringKwarg(kwargs, "body", ""); body != "" {
		bodyReader = strings.NewReader(body)
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, httpMethod, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if headers, ok := kwargs["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}

	client := newH2CClient(30 * time.Second)
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &StepResult{
			StatusCode: 0,
			Success:    false,
			Error:      err.Error(),
			DurationMs: elapsed,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	return &StepResult{
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(respBody)),
		Success:    true,
		DurationMs: elapsed,
		Fields:     map[string]string{"proto": resp.Proto},
	}, nil
}

// ServeMock implements MockHandler for HTTP/2. It reuses the HTTP route
// table and mux from the http protocol (same "METHOD /path" pattern with
// * / ** globs) but serves them over h2c — cleartext HTTP/2 — the mode
// real-world service meshes use between pods behind a TLS-terminating LB.
func (p *http2Protocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mock listen %s: %w", addr, err)
	}

	mux := &mockHTTPMux{routes: spec.Routes, def: spec.Default, emit: emit}
	h2s := &http2.Server{}
	srv := &http.Server{
		Handler:           h2c.NewHandler(mux, h2s),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancel()
		return nil
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// newH2CClient returns an HTTP client that speaks HTTP/2 over cleartext TCP
// (h2c). This is the common mode for service-to-service traffic in service
// meshes and behind TLS-terminating load balancers.
//
// For TLS (h2 on port 443 with ALPN), the default http.Client upgrades
// automatically — but test specs rarely run TLS, so h2c is the useful path.
func newH2CClient(timeout time.Duration) *http.Client {
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// normalizeHTTPURL ensures the address has an http:// prefix.
func normalizeHTTPURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}
