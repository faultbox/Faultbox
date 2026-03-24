package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/faultbox/Faultbox/internal/config"
)

// StepResult captures the outcome of a single step execution.
type StepResult struct {
	Action     string `json:"action"`
	Success    bool   `json:"success"`
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// RunSteps executes trace steps sequentially against running services.
func RunSteps(ctx context.Context, topo *config.TopologyConfig, steps []config.StepConfig, log *slog.Logger) ([]StepResult, error) {
	results := make([]StepResult, 0, len(steps))
	for i, step := range steps {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		var result StepResult
		var err error

		if step.Sleep != nil {
			result, err = runSleepStep(ctx, step.Sleep.Duration)
		} else if step.Action != "" {
			result, err = runActionStep(ctx, topo, step, log)
		} else {
			return results, fmt.Errorf("step[%d]: no action specified", i)
		}

		if err != nil {
			return results, fmt.Errorf("step[%d] %s: %w", i, stepName(step), err)
		}

		log.Info("step completed",
			slog.Int("step", i),
			slog.String("action", stepName(step)),
			slog.Bool("success", result.Success),
			slog.Int64("duration_ms", result.DurationMs),
		)

		results = append(results, result)
		if !result.Success {
			return results, nil
		}
	}
	return results, nil
}

func stepName(s config.StepConfig) string {
	if s.Sleep != nil {
		return fmt.Sprintf("sleep(%s)", s.Sleep.Duration)
	}
	return s.Action
}

func runSleepStep(ctx context.Context, d time.Duration) (StepResult, error) {
	start := time.Now()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return StepResult{}, ctx.Err()
	}
	return StepResult{
		Action:     fmt.Sprintf("sleep(%s)", d),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// runActionStep resolves "service[.interface].operation" and dispatches.
func runActionStep(ctx context.Context, topo *config.TopologyConfig, step config.StepConfig, log *slog.Logger) (StepResult, error) {
	svcName, ifName, operation, err := parseStepAction(step.Action)
	if err != nil {
		return StepResult{}, err
	}

	addr, err := config.ResolveServiceAddr(topo, svcName, ifName)
	if err != nil {
		return StepResult{}, err
	}

	protocol, err := config.ResolveInterfaceProtocol(topo, svcName, ifName)
	if err != nil {
		return StepResult{}, err
	}

	switch protocol {
	case "http":
		return runHTTPStep(ctx, addr, operation, step.Args)
	case "tcp":
		return runTCPStep(ctx, addr, operation, step.Args)
	default:
		return StepResult{}, fmt.Errorf("unsupported protocol %q for step %s", protocol, step.Action)
	}
}

// parseStepAction splits "service.operation" or "service.interface.operation".
func parseStepAction(action string) (service, iface, operation string, err error) {
	parts := strings.Split(action, ".")
	switch len(parts) {
	case 2:
		return parts[0], "", parts[1], nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	default:
		return "", "", "", fmt.Errorf("invalid step action %q: expected service.operation or service.interface.operation", action)
	}
}

// ---------------------------------------------------------------------------
// HTTP step handler
// ---------------------------------------------------------------------------

func runHTTPStep(ctx context.Context, addr, operation string, args map[string]any) (StepResult, error) {
	method := strings.ToUpper(operation)
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH":
	default:
		return StepResult{}, fmt.Errorf("unsupported HTTP operation %q", operation)
	}

	path := getStringArg(args, "path", "/")
	url := fmt.Sprintf("http://%s%s", addr, path)

	var bodyReader io.Reader
	if body := getStringArg(args, "body", ""); body != "" {
		bodyReader = strings.NewReader(body)
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return StepResult{}, fmt.Errorf("create request: %w", err)
	}

	// Apply headers.
	if headers, ok := args["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return StepResult{
			Action:     fmt.Sprintf("HTTP %s %s", method, path),
			Success:    false,
			Error:      err.Error(),
			DurationMs: elapsed,
		}, nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	bodyStr := strings.TrimSpace(string(respBody))

	result := StepResult{
		Action:     fmt.Sprintf("HTTP %s %s", method, path),
		Success:    true,
		StatusCode: resp.StatusCode,
		Body:       bodyStr,
		DurationMs: elapsed,
	}

	// Check expectations.
	if expect, ok := args["expect"].(map[string]any); ok {
		if err := checkHTTPExpect(expect, resp.StatusCode, bodyStr); err != nil {
			result.Success = false
			result.Error = err.Error()
		}
	}

	return result, nil
}

func checkHTTPExpect(expect map[string]any, status int, body string) error {
	if v, ok := expectInt(expect, "status"); ok {
		if status != v {
			return fmt.Errorf("expected status %d, got %d", v, status)
		}
	}
	if v := getStringFromAny(expect, "body_contains"); v != "" {
		if !strings.Contains(body, v) {
			return fmt.Errorf("expected body to contain %q, got %q", v, truncate(body, 200))
		}
	}
	if v := getStringFromAny(expect, "body_equals"); v != "" {
		if body != v {
			return fmt.Errorf("expected body %q, got %q", v, truncate(body, 200))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// TCP step handler
// ---------------------------------------------------------------------------

func runTCPStep(ctx context.Context, addr, operation string, args map[string]any) (StepResult, error) {
	switch operation {
	case "send":
	default:
		return StepResult{}, fmt.Errorf("unsupported TCP operation %q (supported: send)", operation)
	}

	data := getStringArg(args, "data", "")
	if data == "" {
		return StepResult{}, fmt.Errorf("tcp.send requires 'data' argument")
	}

	start := time.Now()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return StepResult{
			Action:     fmt.Sprintf("TCP send to %s", addr),
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Send data (ensure newline terminated).
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	if _, err := fmt.Fprint(conn, data); err != nil {
		return StepResult{
			Action:     fmt.Sprintf("TCP send to %s", addr),
			Success:    false,
			Error:      fmt.Sprintf("write: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// Read one line response.
	scanner := bufio.NewScanner(conn)
	var respLine string
	if scanner.Scan() {
		respLine = strings.TrimSpace(scanner.Text())
	} else if err := scanner.Err(); err != nil {
		return StepResult{
			Action:     fmt.Sprintf("TCP send to %s", addr),
			Success:    false,
			Error:      fmt.Sprintf("read: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	elapsed := time.Since(start).Milliseconds()
	result := StepResult{
		Action:     fmt.Sprintf("TCP send to %s", addr),
		Success:    true,
		Body:       respLine,
		DurationMs: elapsed,
	}

	// Check expectations.
	if expect, ok := args["expect"].(map[string]any); ok {
		if err := checkTCPExpect(expect, respLine); err != nil {
			result.Success = false
			result.Error = err.Error()
		}
	}

	return result, nil
}

func checkTCPExpect(expect map[string]any, response string) error {
	if v := getStringFromAny(expect, "contains"); v != "" {
		if !strings.Contains(response, v) {
			return fmt.Errorf("expected response to contain %q, got %q", v, response)
		}
	}
	if v := getStringFromAny(expect, "equals"); v != "" {
		if response != v {
			return fmt.Errorf("expected response %q, got %q", v, response)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Arg helpers
// ---------------------------------------------------------------------------

func getStringArg(args map[string]any, key, defaultVal string) string {
	if v := getStringFromAny(args, key); v != "" {
		return v
	}
	return defaultVal
}

func getStringFromAny(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func expectInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case int64:
		return int(n), true
	}
	return 0, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
