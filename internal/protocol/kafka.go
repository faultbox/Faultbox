package protocol

import (
	"context"
	"encoding/base64"
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
	return []string{"publish", "consume", "consume_many"}
}

// kafkaReadyTopic is a sentinel topic used to verify broker readiness for produce.
const kafkaReadyTopic = "__faultbox_ready__"

func (p *kafkaProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	addr = ParseAddr(addr).HostPort
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
	case "consume_many":
		return p.consumeMany(ctx, addr, kwargs, start)
	default:
		return nil, fmt.Errorf("unsupported kafka method %q", method)
	}
}

func (p *kafkaProtocol) publish(ctx context.Context, addr string, kwargs map[string]any, start time.Time) (*StepResult, error) {
	topic := getStringKwarg(kwargs, "topic", "")
	if topic == "" {
		return nil, fmt.Errorf("kafka.publish requires topic= argument")
	}
	data, err := kafkaBytes(kwargs, "data")
	if err != nil {
		return nil, err
	}
	key, err := kafkaBytes(kwargs, "key")
	if err != nil {
		return nil, err
	}

	// A step must not reuse DefaultTransport's metadata/connections from a
	// broker that a previous test stopped at the same address (G4). Writers
	// constructed as literals do not own/close that shared transport.
	transport := &kafka.Transport{}
	defer transport.CloseIdleConnections()
	writer := &kafka.Writer{
		Addr:                   kafka.TCP(addr),
		Topic:                  topic,
		BatchTimeout:           100 * time.Millisecond,
		MaxAttempts:            5,
		AllowAutoTopicCreation: true,
		RequiredAcks:           kafka.RequireAll,
		Transport:              transport,
	}
	defer writer.Close()

	var acknowledged kafka.Message
	writer.Completion = func(messages []kafka.Message, err error) {
		if err == nil && len(messages) > 0 {
			acknowledged = messages[0]
		}
	}

	msg := kafka.Message{
		Value: data,
	}
	if len(key) > 0 {
		msg.Key = key
	} else if binaryKey, ok := kwargs["key"].([]byte); ok {
		// Kafka distinguishes an explicit empty binary key from a null key.
		msg.Key = binaryKey
	}

	// Retry loop to handle transient errors on first publish:
	//   - "Unknown Topic Or Partition": auto-create in progress, metadata not propagated yet.
	//   - "Not Leader": partition leader election in progress.
	//   - "unexpected EOF": broker restarted or not ready yet.
	// Each attempt waits 1s before retrying, allowing Kafka time to converge.
	err = nil
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

	body, _ := json.Marshal(map[string]any{"published": true, "topic": topic, "partition": acknowledged.Partition, "offset": acknowledged.Offset, "key_base64": base64.StdEncoding.EncodeToString(msg.Key), "key_is_null": msg.Key == nil})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func kafkaBytes(kwargs map[string]any, name string) ([]byte, error) {
	v, ok := kwargs[name]
	if !ok {
		// The documented default is an empty value, not a tombstone (nil).
		return []byte{}, nil
	}
	switch b := v.(type) {
	case string:
		return []byte(b), nil
	case []byte:
		return b, nil
	default:
		return nil, fmt.Errorf("kafka.publish %s must be a string or bytes (got %T)", name, v)
	}
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
		"topic":        msg.Topic,
		"partition":    msg.Partition,
		"offset":       msg.Offset,
		"key":          string(msg.Key),
		"value":        string(msg.Value),
		"key_base64":   base64.StdEncoding.EncodeToString(msg.Key),
		"value_base64": base64.StdEncoding.EncodeToString(msg.Value),
	})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}
