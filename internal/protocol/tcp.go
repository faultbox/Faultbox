package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func init() {
	Register(&tcpProtocol{})
}

type tcpProtocol struct{}

func (p *tcpProtocol) Name() string { return "tcp" }

func (p *tcpProtocol) Methods() []string {
	return []string{"send"}
}

func (p *tcpProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return TCPHealthcheck(ctx, addr, timeout)
}

func (p *tcpProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	if method != "send" {
		return nil, fmt.Errorf("unsupported TCP method %q (supported: send)", method)
	}

	data := getStringKwarg(kwargs, "data", "")
	if data == "" {
		return nil, fmt.Errorf("tcp.send requires 'data' argument")
	}

	start := time.Now()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	if _, err := fmt.Fprint(conn, data); err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("write: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	scanner := bufio.NewScanner(conn)
	var respLine string
	if scanner.Scan() {
		respLine = strings.TrimSpace(scanner.Text())
	} else if err := scanner.Err(); err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("read: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	return &StepResult{
		Body:       respLine,
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}
