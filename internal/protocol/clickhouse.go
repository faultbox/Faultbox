package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func init() {
	Register(&clickhouseProtocol{})
}

// clickhouseProtocol uses ClickHouse's HTTP interface (default port 8123).
// The native binary protocol (port 9000) is more efficient but far harder
// to proxy; HTTP is the simplification flagged in RFC-016.
type clickhouseProtocol struct{}

func (p *clickhouseProtocol) Name() string { return "clickhouse" }

func (p *clickhouseProtocol) Methods() []string {
	return []string{"query", "exec"}
}

func (p *clickhouseProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	healthURL := fmt.Sprintf("http://%s/ping", addr)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("clickhouse healthcheck %q timed out after %s", addr, timeout)
}

func (p *clickhouseProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	sql := getStringKwarg(kwargs, "sql", "")
	if sql == "" {
		return nil, fmt.Errorf("clickhouse.%s requires sql= argument", method)
	}

	start := time.Now()
	switch method {
	case "query":
		return p.executeQuery(ctx, addr, sql, start)
	case "exec":
		return p.executeExec(ctx, addr, sql, start)
	default:
		return nil, fmt.Errorf("unsupported clickhouse method %q", method)
	}
}

func (p *clickhouseProtocol) executeQuery(ctx context.Context, addr, sql string, start time.Time) (*StepResult, error) {
	// Append FORMAT JSON so ClickHouse returns structured JSON we can parse
	// directly — unless the user already specified a format.
	if !strings.Contains(strings.ToUpper(sql), "FORMAT ") {
		sql = strings.TrimRight(sql, " ;") + " FORMAT JSON"
	}

	body, statusCode, err := p.postSQL(ctx, addr, sql, 30*time.Second)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &StepResult{Success: false, Error: err.Error(), DurationMs: elapsed}, nil
	}
	if statusCode != 200 {
		return &StepResult{Success: false, StatusCode: statusCode, Error: strings.TrimSpace(string(body)), DurationMs: elapsed}, nil
	}

	// ClickHouse JSON format: {"meta":[...], "data":[...], "rows":N, ...}.
	// Return the data rows directly for consistency with other SQL plugins.
	var parsed struct {
		Data []map[string]any `json:"data"`
		Rows int              `json:"rows"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return &StepResult{Success: false, Error: fmt.Sprintf("parse: %v", err), DurationMs: elapsed}, nil
	}
	rendered, _ := json.Marshal(parsed.Data)
	return &StepResult{
		Body:       string(rendered),
		Success:    true,
		DurationMs: elapsed,
		Fields:     map[string]string{"rows": fmt.Sprintf("%d", parsed.Rows)},
	}, nil
}

func (p *clickhouseProtocol) executeExec(ctx context.Context, addr, sql string, start time.Time) (*StepResult, error) {
	body, statusCode, err := p.postSQL(ctx, addr, sql, 30*time.Second)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &StepResult{Success: false, Error: err.Error(), DurationMs: elapsed}, nil
	}
	if statusCode != 200 {
		return &StepResult{Success: false, StatusCode: statusCode, Error: strings.TrimSpace(string(body)), DurationMs: elapsed}, nil
	}
	return &StepResult{
		StatusCode: statusCode,
		Body:       `{"ok":true}`,
		Success:    true,
		DurationMs: elapsed,
	}, nil
}

func (p *clickhouseProtocol) postSQL(ctx context.Context, addr, sql string, timeout time.Duration) ([]byte, int, error) {
	endpoint := fmt.Sprintf("http://%s/?%s", addr, url.Values{"default_format": []string{"JSON"}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(sql))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	return body, resp.StatusCode, nil
}
