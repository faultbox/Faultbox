package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func startConsumeBroker(t *testing.T) (*kafkaProtocol, string) {
	t.Helper()
	p := &kafkaProtocol{}
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- p.ServeMock(ctx, addr, MockSpec{Config: map[string]any{"topics": map[string]any{"events": nil, "empty": nil}}}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("mock did not stop")
		}
	})
	if err := waitKafkaMockReady(addr, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	return p, addr
}

func produceConsumeRecord(t *testing.T, p *kafkaProtocol, addr string, value, key []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := p.ExecuteStep(ctx, addr, "publish", map[string]any{"topic": "events", "data": value, "key": key})
	if err != nil || !r.Success {
		t.Fatalf("publish: %+v %v", r, err)
	}
}

type batchResult struct {
	Records    []consumedKafkaRecord `json:"records"`
	StopReason string                `json:"stop_reason"`
	Committed  int                   `json:"committed_records"`
	Assigned   bool                  `json:"assigned"`
}

func readConsumeBatch(t *testing.T, p *kafkaProtocol, ctx context.Context, addr string, args map[string]any) (*StepResult, batchResult) {
	t.Helper()
	result, err := p.ExecuteStep(ctx, addr, "consume_many", args)
	if err != nil {
		t.Fatal(err)
	}
	var body batchResult
	if err := json.Unmarshal([]byte(result.Body), &body); err != nil {
		t.Fatal(err)
	}
	return result, body
}

func TestKafkaConsumeManyTypedBatchAndResume(t *testing.T) {
	p, addr := startConsumeBroker(t)
	path := writeFds(t, buildSettingDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatal(err)
	}
	_, desc, err := ResolveMethod(files, "/test.config.ConfigService/GetSetting")
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]any{"id": "9007199254740993", "name": strings.Repeat("я", 130) + fmt.Sprint(i)})
		wire, err := JSONToTypedMessage(files, desc, body)
		if err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, wire)
		produceConsumeRecord(t, p, addr, wire, []byte{0, 255, byte(i)})
	}
	args := map[string]any{"topic": "events", "group": "typed-observer", "descriptors": path, "message": "test.config.Setting", "max_records": 2, "timeout": "3s", "idle_timeout": "100ms"}
	result, first := readConsumeBatch(t, p, context.Background(), addr, args)
	if !result.Success || first.StopReason != "max_records" || len(first.Records) != 2 || first.Committed != 2 {
		t.Fatalf("first=%+v err=%s", first, result.Error)
	}
	args["max_records"] = 10
	result, second := readConsumeBatch(t, p, context.Background(), addr, args)
	if !result.Success || second.StopReason != "idle_timeout" || len(second.Records) != 1 || second.Committed != 1 {
		t.Fatalf("second=%+v err=%s", second, result.Error)
	}
	for i, record := range append(first.Records, second.Records...) {
		if record.Offset != int64(i) || record.Partition != 0 {
			t.Fatalf("wrong offset: %+v", record)
		}
		wire, err := base64.StdEncoding.DecodeString(record.ValueBase64)
		if err != nil || string(wire) != string(payloads[i]) {
			t.Fatalf("bytes corrupted: %x %v", wire, err)
		}
		if record.KeyBase64 != base64.StdEncoding.EncodeToString([]byte{0, 255, byte(i)}) {
			t.Fatal("key corrupted")
		}
		var decoded map[string]any
		if err := json.Unmarshal(record.Data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["id"] != "9007199254740993" || decoded["name"] != strings.Repeat("я", 130)+fmt.Sprint(i) {
			t.Fatalf("decoded=%v", decoded)
		}
	}
}

func TestKafkaConsumeManyEmptyTimeoutAndCancellation(t *testing.T) {
	p, addr := startConsumeBroker(t)
	args := map[string]any{"topic": "empty", "group": "empty-observer", "timeout": "2s", "idle_timeout": "50ms"}
	result, body := readConsumeBatch(t, p, context.Background(), addr, args)
	if !result.Success || body.StopReason != "idle_timeout" || len(body.Records) != 0 || !body.Assigned {
		t.Fatalf("empty=%+v %s", body, result.Error)
	}
	if result.DurationMs >= 1800 {
		t.Fatalf("empty read took %dms", result.DurationMs)
	}
	args["timeout"] = "250ms"
	args["idle_timeout"] = "3s"
	result, body = readConsumeBatch(t, p, context.Background(), addr, args)
	if !result.Success || body.StopReason != "timeout" {
		t.Fatalf("bounded read=%+v %s", body, result.Error)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, body = readConsumeBatch(t, p, ctx, addr, args)
	if result.Success || body.StopReason != "cancelled" {
		t.Fatalf("cancel=%+v %s", body, result.Error)
	}
	result, body = readConsumeBatch(t, p, context.Background(), freeLoopbackAddr(t), args)
	if result.Success || body.Assigned {
		t.Fatalf("unreachable broker reported empty success: %+v", body)
	}
}

func TestKafkaConsumeManyMalformedRecordIsNotCommitted(t *testing.T) {
	p, addr := startConsumeBroker(t)
	path := writeFds(t, buildSettingDescriptorSet())
	produceConsumeRecord(t, p, addr, []byte{8, 128}, nil)
	args := map[string]any{"topic": "events", "group": "decode-errors", "descriptors": path, "message": "test.config.Setting", "max_records": 1, "timeout": "2s"}
	result, body := readConsumeBatch(t, p, context.Background(), addr, args)
	if result.Success || body.StopReason != "decode_error" || body.Committed != 0 || !strings.Contains(result.Error, "offset 0") {
		t.Fatalf("bad=%+v %s", body, result.Error)
	}
	delete(args, "descriptors")
	delete(args, "message")
	result, body = readConsumeBatch(t, p, context.Background(), addr, args)
	if !result.Success || len(body.Records) != 1 || body.Records[0].Offset != 0 {
		t.Fatalf("record was skipped: %+v %s", body, result.Error)
	}
}

func TestKafkaConsumeManyPreservesEmptyAndNull(t *testing.T) {
	p, addr := startConsumeBroker(t)
	produceConsumeRecord(t, p, addr, []byte{}, []byte{})
	produceConsumeRecord(t, p, addr, nil, nil)
	result, body := readConsumeBatch(t, p, context.Background(), addr, map[string]any{"topic": "events", "max_records": 2, "timeout": "2s"})
	if !result.Success || len(body.Records) != 2 {
		t.Fatalf("%+v %s", body, result.Error)
	}
	if body.Records[0].ValueIsNull || body.Records[0].KeyIsNull || !body.Records[1].ValueIsNull || !body.Records[1].KeyIsNull {
		t.Fatalf("empty/null lost: %+v", body.Records)
	}
}
