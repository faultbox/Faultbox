package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/twmb/franz-go/pkg/kgo"
)

type consumedKafkaRecord struct {
	Topic       string          `json:"topic"`
	Partition   int32           `json:"partition"`
	Offset      int64           `json:"offset"`
	Key         any             `json:"key"`
	Value       any             `json:"value"`
	KeyBase64   string          `json:"key_base64"`
	ValueBase64 string          `json:"value_base64"`
	KeyIsNull   bool            `json:"key_is_null"`
	ValueIsNull bool            `json:"value_is_null"`
	Data        json.RawMessage `json:"data,omitempty"`
}

func kafkaRecord(r *kgo.Record) consumedKafkaRecord {
	out := consumedKafkaRecord{Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, KeyBase64: base64.StdEncoding.EncodeToString(r.Key), ValueBase64: base64.StdEncoding.EncodeToString(r.Value), KeyIsNull: r.Key == nil, ValueIsNull: r.Value == nil}
	if r.Key != nil && utf8.Valid(r.Key) {
		out.Key = string(r.Key)
	}
	if r.Value != nil && utf8.Valid(r.Value) {
		out.Value = string(r.Value)
	}
	return out
}

func (p *kafkaProtocol) consumeMany(ctx context.Context, addr string, args map[string]any, start time.Time) (result *StepResult, err error) {
	allowed := map[string]bool{"topic": true, "group": true, "max_records": true, "timeout": true, "idle_timeout": true, "descriptors": true, "message": true}
	for key := range args {
		if !allowed[key] {
			return nil, fmt.Errorf("kafka.consume_many: unknown argument %q", key)
		}
	}
	topic, err := kafkaStringArg(args, "topic", "")
	if err != nil {
		return nil, err
	}
	if topic == "" {
		return nil, fmt.Errorf("kafka.consume_many requires topic=")
	}
	group, err := kafkaStringArg(args, "group", "faultbox")
	if err != nil {
		return nil, err
	}
	if group == "" {
		return nil, fmt.Errorf("kafka.consume_many group must not be empty")
	}
	maxRecords := int64(100)
	if v, ok := args["max_records"]; ok {
		switch n := v.(type) {
		case int:
			maxRecords = int64(n)
		case int64:
			maxRecords = n
		default:
			return nil, fmt.Errorf("kafka.consume_many max_records must be an integer")
		}
	}
	if maxRecords < 1 || maxRecords > 10000 {
		return nil, fmt.Errorf("kafka.consume_many max_records must be between 1 and 10000")
	}
	timeout, err := kafkaReadDuration(args, "timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	idle, err := kafkaReadDuration(args, "idle_timeout", 250*time.Millisecond)
	if err != nil {
		return nil, err
	}
	path, err := kafkaStringArg(args, "descriptors", "")
	if err != nil {
		return nil, err
	}
	message, err := kafkaStringArg(args, "message", "")
	if err != nil {
		return nil, err
	}
	if (path == "") != (message == "") {
		return nil, fmt.Errorf("kafka.consume_many requires descriptors and message together")
	}
	var decode func([]byte) (json.RawMessage, error)
	if path != "" {
		decode, err = LoadProtoDecoder(path, message)
		if err != nil {
			return nil, err
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var assigned atomic.Bool
	client, err := kgo.NewClient(kgo.SeedBrokers(ParseAddr(addr).HostPort), kgo.ConsumerGroup(group), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.DisableAutoCommit(), kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxWait(50*time.Millisecond), kgo.AllowAutoTopicCreation(),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, parts map[string][]int32) {
			assigned.Store(len(parts[topic]) > 0)
		}),
		kgo.OnPartitionsRevoked(func(context.Context, *kgo.Client, map[string][]int32) { assigned.Store(false) }),
		kgo.OnPartitionsLost(func(context.Context, *kgo.Client, map[string][]int32) { assigned.Store(false) }))
	if err != nil {
		return nil, err
	}
	defer func() {
		client.AllowRebalance()
		// Do not let a dead coordinator turn a bounded observation into an
		// unbounded LeaveGroup wait. Offsets were already committed explicitly.
		leaveCtx, leaveCancel := context.WithTimeout(readCtx, 100*time.Millisecond)
		_ = client.LeaveGroupContext(leaveCtx)
		leaveCancel()
		client.Close()
		if result != nil {
			result.DurationMs = time.Since(start).Milliseconds()
		}
	}()
	records := make([]consumedKafkaRecord, 0)
	committed := 0
	finish := func(reason string, err error) (*StepResult, error) {
		body, marshalErr := json.Marshal(map[string]any{"records": records, "stop_reason": reason, "group": group, "assigned": assigned.Load(), "committed_records": committed})
		if marshalErr != nil {
			return nil, marshalErr
		}
		result := &StepResult{Success: err == nil, Body: string(body), DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			result.Error = err.Error()
		}
		return result, nil
	}
	var idleDeadline time.Time
	for {
		if err := ctx.Err(); err != nil {
			return finish("cancelled", err)
		}
		if readCtx.Err() != nil {
			if !assigned.Load() {
				return finish("timeout", fmt.Errorf("Kafka consumer was not assigned before timeout"))
			}
			return finish("timeout", nil)
		}
		// Joining a real consumer group may take seconds. Do not call that an
		// empty-topic idle timeout: the idle budget starts after assignment.
		pollBudget := 50 * time.Millisecond
		if assigned.Load() {
			if idleDeadline.IsZero() {
				idleDeadline = time.Now().Add(idle)
			}
			pollBudget = time.Until(idleDeadline)
			if pollBudget <= 0 {
				return finish("idle_timeout", nil)
			}
		} else {
			idleDeadline = time.Time{}
		}
		pollCtx, pollCancel := context.WithTimeout(readCtx, pollBudget)
		fetches := client.PollRecords(pollCtx, int(maxRecords)-len(records))
		pollCancel()
		batch := fetches.Records()
		for _, failure := range fetches.Errors() {
			if errors.Is(failure.Err, context.DeadlineExceeded) || errors.Is(failure.Err, context.Canceled) {
				continue
			}
			return finish("error", fmt.Errorf("Kafka fetch %s[%d]: %w", failure.Topic, failure.Partition, failure.Err))
		}
		for _, r := range batch {
			record := kafkaRecord(r)
			if decode != nil {
				if r.Value == nil {
					record.Data = json.RawMessage("null")
				} else {
					record.Data, err = decode(r.Value)
					if err != nil {
						records = append(records, record)
						return finish("decode_error", fmt.Errorf("decode %s[%d] offset %d as %s: %w", r.Topic, r.Partition, r.Offset, message, err))
					}
				}
			}
			records = append(records, record)
		}
		if len(batch) > 0 {
			// Commit only after every returned record has decoded successfully.
			if err := client.CommitRecords(readCtx, batch...); err != nil {
				return finish("commit_error", err)
			}
			committed += len(batch)
			idleDeadline = time.Now().Add(idle)
		}
		client.AllowRebalance()
		if len(records) >= int(maxRecords) {
			return finish("max_records", nil)
		}
	}
}

func kafkaStringArg(args map[string]any, key, def string) (string, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("kafka.consume_many %s must be a string", key)
	}
	return s, nil
}
func kafkaReadDuration(args map[string]any, key string, def time.Duration) (time.Duration, error) {
	s, err := kafkaStringArg(args, key, def.String())
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("kafka.consume_many %s must be a positive duration", key)
	}
	return d, nil
}
