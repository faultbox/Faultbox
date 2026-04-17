package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// kafkaReadyTopic is a sentinel topic used to verify broker readiness for produce.
const kafkaReadyTopic = "__faultbox_ready__"

func (p *kafkaProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	// Strong readiness check: we don't just verify metadata (which succeeds as
	// soon as docker-proxy is up), but also verify the broker can handle a
	// produce request by dialing the partition leader for a sentinel topic.
	// This ensures Kafka is fully initialised (controller elected, log dirs
	// mounted) before the test proceeds.
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		// Phase 1: basic metadata check.
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := kafka.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err != nil {
			lastErr = err
		} else {
			// Ensure the sentinel topic exists.
			_ = conn.CreateTopics(kafka.TopicConfig{
				Topic:             kafkaReadyTopic,
				NumPartitions:     1,
				ReplicationFactor: 1,
			})
			conn.Close()

			// Phase 2: dial the partition leader — fails until leader is elected.
			leaderCtx, leaderCancel := context.WithTimeout(ctx, 3*time.Second)
			leaderConn, leaderErr := kafka.DialLeader(leaderCtx, "tcp", addr, kafkaReadyTopic, 0)
			leaderCancel()
			if leaderErr == nil {
				leaderConn.Close()
				return nil // broker is fully ready
			}
			lastErr = leaderErr
		}
		// Pause before retrying.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("kafka not ready at %s after %s: %w", addr, timeout, lastErr)
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
		Addr:                   kafka.TCP(addr),
		Topic:                  topic,
		BatchTimeout:           100 * time.Millisecond,
		MaxAttempts:            5,
		AllowAutoTopicCreation: true,
	}
	defer writer.Close()

	msg := kafka.Message{
		Value: []byte(data),
	}
	if key != "" {
		msg.Key = []byte(key)
	}

	// Retry loop to handle transient errors on first publish:
	//   - "Unknown Topic Or Partition": auto-create in progress, metadata not propagated yet.
	//   - "Not Leader": partition leader election in progress.
	//   - "unexpected EOF": broker restarted or not ready yet.
	// Each attempt waits 1s before retrying, allowing Kafka time to converge.
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		err = writer.WriteMessages(ctx, msg)
		if err == nil {
			break
		}
		errStr := err.Error()
		if strings.Contains(errStr, "Unknown Topic") ||
			strings.Contains(errStr, "Not Leader") ||
			strings.Contains(errStr, "unexpected EOF") ||
			strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "connection reset") ||
			strings.Contains(errStr, "EOF") {
			select {
			case <-ctx.Done():
				break
			case <-time.After(1000 * time.Millisecond):
			}
			continue
		}
		break
	}

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
