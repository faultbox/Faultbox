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

func (p *natsProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return TCPHealthcheck(ctx, addr, timeout)
}

func (p *natsProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	nc, err := nats.Connect(fmt.Sprintf("nats://%s", addr),
		nats.Timeout(5*time.Second))
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
	nc.Flush()

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
