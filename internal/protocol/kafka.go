package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

func init() {
	Register(&kafkaProtocol{})
}

type kafkaProtocol struct{}

func (p *kafkaProtocol) Name() string { return "kafka" }

func (p *kafkaProtocol) Methods() []string {
	return []string{"publish", "consume"}
}

func (p *kafkaProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return TCPHealthcheck(ctx, addr, timeout)
}

func (p *kafkaProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	start := time.Now()

	switch method {
	case "publish":
		return p.publish(ctx, addr, kwargs, start)
	case "consume":
		return p.consume(ctx, addr, kwargs, start)
	default:
		return nil, fmt.Errorf("unsupported kafka method %q", method)
	}
}

func (p *kafkaProtocol) publish(ctx context.Context, addr string, kwargs map[string]any, start time.Time) (*StepResult, error) {
	topic := getStringKwarg(kwargs, "topic", "")
	if topic == "" {
		return nil, fmt.Errorf("kafka.publish requires topic= argument")
	}
	data := getStringKwarg(kwargs, "data", "")
	key := getStringKwarg(kwargs, "key", "")

	writer := &kafka.Writer{
		Addr:         kafka.TCP(addr),
		Topic:        topic,
		BatchTimeout: 100 * time.Millisecond,
	}
	defer writer.Close()

	msg := kafka.Message{
		Value: []byte(data),
	}
	if key != "" {
		msg.Key = []byte(key)
	}

	err := writer.WriteMessages(ctx, msg)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	body, _ := json.Marshal(map[string]any{"published": true, "topic": topic})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (p *kafkaProtocol) consume(ctx context.Context, addr string, kwargs map[string]any, start time.Time) (*StepResult, error) {
	topic := getStringKwarg(kwargs, "topic", "")
	if topic == "" {
		return nil, fmt.Errorf("kafka.consume requires topic= argument")
	}
	group := getStringKwarg(kwargs, "group", "faultbox")

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{addr},
		Topic:    topic,
		GroupID:  group,
		MaxWait:  5 * time.Second,
		MinBytes: 1,
		MaxBytes: 10e6,
	})
	defer reader.Close()

	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	msg, err := reader.ReadMessage(readCtx)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	body, _ := json.Marshal(map[string]any{
		"topic":     msg.Topic,
		"partition": msg.Partition,
		"offset":    msg.Offset,
		"key":       string(msg.Key),
		"value":     string(msg.Value),
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}
