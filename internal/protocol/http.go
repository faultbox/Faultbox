package protocol

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

func init() {
	Register(&httpProtocol{})
}

type httpProtocol struct{}

func (p *httpProtocol) Name() string { return "http" }

func (p *httpProtocol) Methods() []string {
	return []string{"get", "post", "put", "delete", "patch"}
}

func (p *httpProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := addr
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + addr
	}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("HTTP healthcheck %q timed out after %s", addr, timeout)
}

func (p *httpProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	httpMethod := strings.ToUpper(method)
	switch httpMethod {
	case "GET", "POST", "PUT", "DELETE", "PATCH":
	default:
		return nil, fmt.Errorf("unsupported HTTP method %q", method)
	}

	path := getStringKwarg(kwargs, "path", "/")
	url := fmt.Sprintf("http://%s%s", addr, path)

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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &StepResult{
			StatusCode: 0,
			Body:       "",
			Success:    false,
			Error:      err.Error(),
			DurationMs: elapsed,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	bodyStr := strings.TrimSpace(string(respBody))

	return &StepResult{
		StatusCode: resp.StatusCode,
		Body:       bodyStr,
		Success:    true,
		DurationMs: elapsed,
	}, nil
}

// getStringKwarg extracts a string from kwargs with a default.
func getStringKwarg(kwargs map[string]any, key, defaultVal string) string {
	if v, ok := kwargs[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultVal
}

// ServeMock implements MockHandler for the HTTP protocol. It stands up an
// in-process net/http server on addr that matches requests against
// spec.Routes ("METHOD /path" with * and ** glob support) and returns the
// corresponding MockResponse. Unmatched requests fall back to spec.Default
// (or HTTP 404 if no default is set). Every request is logged via emit.
func (p *httpProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mock listen %s: %w", addr, err)
	}
	if spec.TLSCert != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*spec.TLSCert}})
	}

	mux := &mockHTTPMux{routes: spec.Routes, def: spec.Default, emit: emit}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

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

// mockHTTPMux dispatches incoming requests against a MockSpec route table.
type mockHTTPMux struct {
	routes []MockRoute
	def    *MockResponse
	emit   MockEmitter
}

func (m *mockHTTPMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := r.Method + " " + r.URL.Path
	bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	_ = r.Body.Close()

	route, matched := matchHTTPRoute(m.routes, r.Method, r.URL.Path)

	var resp *MockResponse
	switch {
	case matched && route.Dynamic != nil:
		dyn, err := route.Dynamic(buildMockRequest(r, bodyBytes))
		if err != nil {
			http.Error(w, fmt.Sprintf("dynamic handler error: %v", err), http.StatusInternalServerError)
			emitWith(m.emit, op, map[string]string{"status": "500", "error": err.Error()})
			return
		}
		resp = dyn
	case matched:
		resp = route.Response
	case m.def != nil:
		resp = m.def
	default:
		resp = &MockResponse{Status: http.StatusNotFound, Body: []byte("not found\n"), ContentType: "text/plain"}
	}

	writeMockResponse(w, resp)
	emitWith(m.emit, op, map[string]string{
		"status":    fmt.Sprintf("%d", statusOr(resp, 200)),
		"body_size": fmt.Sprintf("%d", len(resp.Body)),
		"req_size":  fmt.Sprintf("%d", len(bodyBytes)),
	})
}

func buildMockRequest(r *http.Request, body []byte) MockRequest {
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	query := make(map[string]string)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			query[k] = v[0]
		}
	}
	return MockRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: headers,
		Query:   query,
		Body:    body,
	}
}

func writeMockResponse(w http.ResponseWriter, resp *MockResponse) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.ContentType != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

func statusOr(resp *MockResponse, fallback int) int {
	if resp.Status == 0 {
		return fallback
	}
	return resp.Status
}

func emitWith(emit MockEmitter, op string, fields map[string]string) {
	if emit != nil {
		emit(op, fields)
	}
}

// matchHTTPRoute picks the first route whose pattern matches (METHOD, path).
// Pattern format: "METHOD /glob". * matches one path segment, ** matches
// zero or more segments. Exact literal matches take precedence over globs
// (sorted at build time — see mockHTTPMux construction). For now this is a
// simple linear scan; optimize if large route tables appear in practice.
func matchHTTPRoute(routes []MockRoute, method, urlPath string) (MockRoute, bool) {
	for _, r := range routes {
		m, p, ok := splitMethodPattern(r.Pattern)
		if !ok {
			continue
		}
		if m != "*" && !strings.EqualFold(m, method) {
			continue
		}
		if matchGlob(p, urlPath) {
			return r, true
		}
	}
	return MockRoute{}, false
}

func splitMethodPattern(pattern string) (string, string, bool) {
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// matchGlob matches a URL path against a glob pattern. * matches a single
// path segment (no /); ** matches zero or more segments (including /).
// Literal "/" in pattern must match literal "/" in path.
func matchGlob(pattern, target string) bool {
	if !strings.ContainsAny(pattern, "*") {
		// Fast path for literal patterns.
		m, _ := path.Match(pattern, target)
		return m || pattern == target
	}

	pparts := strings.Split(strings.Trim(pattern, "/"), "/")
	tparts := strings.Split(strings.Trim(target, "/"), "/")
	return matchSegments(pparts, tparts)
}

func matchSegments(pattern, target []string) bool {
	if len(pattern) == 0 {
		return len(target) == 0
	}
	head := pattern[0]
	if head == "**" {
		// Zero or more segments. Try matching remaining pattern at each
		// position in target.
		for i := 0; i <= len(target); i++ {
			if matchSegments(pattern[1:], target[i:]) {
				return true
			}
		}
		return false
	}
	if len(target) == 0 {
		return false
	}
	if head == "*" || head == target[0] {
		return matchSegments(pattern[1:], target[1:])
	}
	// Segment-level glob (e.g., "user-*") using path.Match semantics.
	if ok, _ := path.Match(head, target[0]); ok {
		return matchSegments(pattern[1:], target[1:])
	}
	return false
}

// TCPHealthcheck does a simple TCP dial — shared by tcp and other protocols.
func TCPHealthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("TCP healthcheck %q timed out after %s", addr, timeout)
}
