package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

func init() {
	Register(&natsProtocol{})
}

type natsProtocol struct{}

func (p *natsProtocol) Name() string { return "nats" }

func (p *natsProtocol) Methods() []string {
	return []string{"publish", "request", "subscribe"}
}

// Healthcheck reports whether NATS is ready to accept publishes.
//
// This was a bare TCP connect, which reports ready the moment nats-server binds
// its port — before it will serve. The window is short but real: roughly one run
// in four, the first publish through the proxy failed with a bare `EOF` because
// the server accepted the connection and immediately closed it.
//
// A full client connect instead completes NATS's own handshake (INFO →
// CONNECT → PING → PONG) and a Flush proves the round trip, so "ready" means
// the server answered.
func (p *natsProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	a := ParseAddr(addr)
	return ReadyAfterTCP(ctx, "nats", a.HostPort, timeout, func(context.Context) error {
		opts := []nats.Option{nats.Timeout(2 * time.Second)}
		if a.User != "" {
			opts = append(opts, nats.UserInfo(a.User, a.Password))
		}
		nc, err := nats.Connect(fmt.Sprintf("nats://%s", a.HostPort), opts...)
		if err != nil {
			return fmt.Errorf("nats connect: %w", err)
		}
		defer nc.Close()
		if err := nc.FlushTimeout(2 * time.Second); err != nil {
			return fmt.Errorf("nats flush: %w", err)
		}
		return nil
	})
}

func (p *natsProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	a := CredentialsFor(addr, kwargs)
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if a.User != "" {
		opts = append(opts, nats.UserInfo(a.User, a.Password))
	}
	nc, err := nats.Connect(fmt.Sprintf("nats://%s", a.HostPort), opts...)
	if err != nil {
		return &StepResult{Success: false, Error: err.Error()}, nil
	}
	defer nc.Close()

	start := time.Now()
	switch method {
	case "publish":
		return p.publish(nc, kwargs, start)
	case "request":
		return p.request(nc, kwargs, start)
	case "subscribe":
		return p.subscribe(nc, kwargs, start)
	default:
		return nil, fmt.Errorf("unsupported nats method %q", method)
	}
}

func (p *natsProtocol) publish(nc *nats.Conn, kwargs map[string]any, start time.Time) (*StepResult, error) {
	subject := getStringKwarg(kwargs, "subject", "")
	if subject == "" {
		return nil, fmt.Errorf("nats.publish requires subject= argument")
	}
	data := getStringKwarg(kwargs, "data", "")

	if err := nc.Publish(subject, []byte(data)); err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	// NATS publishing is fire-and-forget into a client-side buffer, so Publish
	// returning nil means "queued", not "delivered". The flush is what makes it
	// a round trip — and its error was previously discarded, so a publish that
	// never reached the server still reported Success: true. That is the same
	// silently-successful step this audit exists to eliminate.
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("publish not confirmed by server: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	body, _ := json.Marshal(map[string]any{"published": true, "subject": subject})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *natsProtocol) request(nc *nats.Conn, kwargs map[string]any, start time.Time) (*StepResult, error) {
	subject := getStringKwarg(kwargs, "subject", "")
	if subject == "" {
		return nil, fmt.Errorf("nats.request requires subject= argument")
	}
	data := getStringKwarg(kwargs, "data", "")

	msg, err := nc.Request(subject, []byte(data), 5*time.Second)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	body, _ := json.Marshal(map[string]any{
		"subject": msg.Subject,
		"data":    string(msg.Data),
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *natsProtocol) subscribe(nc *nats.Conn, kwargs map[string]any, start time.Time) (*StepResult, error) {
	subject := getStringKwarg(kwargs, "subject", "")
	if subject == "" {
		return nil, fmt.Errorf("nats.subscribe requires subject= argument")
	}

	ch := make(chan *nats.Msg, 1)
	sub, err := nc.ChanSubscribe(subject, ch)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer sub.Unsubscribe()

	select {
	case msg := <-ch:
		body, _ := json.Marshal(map[string]any{
			"subject": msg.Subject,
			"data":    string(msg.Data),
		})
		return &StepResult{
			Body:       string(body),
			Success:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	case <-time.After(10 * time.Second):
		return &StepResult{
			Success:    false,
			Error:      "subscribe timeout: no message received",
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
}
