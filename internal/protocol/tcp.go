package protocol

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
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

// ServeMock implements MockHandler for TCP. Each accepted connection is
// handled on its own goroutine: the handler reads one newline-terminated
// line, matches it against route patterns as byte prefixes, writes the
// corresponding response, and closes the connection.
//
// Patterns and responses work on raw bytes — typically line-framed ASCII
// ("PING\n" -> "PONG\n") which is the common pattern for legacy TCP tools.
// For non-line-framed protocols, users should use a protocol-specific mock
// from @faultbox/mocks/<name>.star.
func (p *tcpProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mock listen %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go handleTCPMockConn(ctx, conn, spec, emit)
	}
}

func handleTCPMockConn(ctx context.Context, conn net.Conn, spec MockSpec, emit MockEmitter) {
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		emitWith(emit, "recv_error", map[string]string{"error": err.Error()})
		return
	}

	route, matched := matchBytesRoute(spec.Routes, line)

	var resp *MockResponse
	switch {
	case matched && route.Dynamic != nil:
		dyn, derr := route.Dynamic(MockRequest{Body: line})
		if derr != nil {
			emitWith(emit, "dynamic_error", map[string]string{"error": derr.Error()})
			return
		}
		resp = dyn
	case matched:
		resp = route.Response
	case spec.Default != nil:
		resp = spec.Default
	}

	if resp != nil && len(resp.Body) > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, werr := conn.Write(resp.Body); werr != nil {
			emitWith(emit, "write_error", map[string]string{"error": werr.Error()})
			return
		}
	}
	emitWith(emit, "recv", map[string]string{
		"req_size":  fmt.Sprintf("%d", len(line)),
		"resp_size": fmt.Sprintf("%d", bodyLen(resp)),
		"matched":   fmt.Sprintf("%t", matched),
	})
}

// matchBytesRoute returns the first route whose pattern is a byte prefix of
// target. Used by TCP and UDP mock handlers.
func matchBytesRoute(routes []MockRoute, target []byte) (MockRoute, bool) {
	for _, r := range routes {
		if bytes.HasPrefix(target, []byte(r.Pattern)) {
			return r, true
		}
	}
	return MockRoute{}, false
}

func bodyLen(resp *MockResponse) int {
	if resp == nil {
		return 0
	}
	return len(resp.Body)
}
