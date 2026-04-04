package protocol

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
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
