package protocol

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestKafkaBinaryRecordsAndCommitEvents(t *testing.T) {
	p := &kafkaProtocol{}
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var mu sync.Mutex
	var events []struct {
		op     string
		fields map[string]string
	}
	emit := func(op string, fields map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, struct {
			op     string
			fields map[string]string
		}{op, fields})
	}
	done := make(chan error, 1)
	go func() {
		done <- p.ServeMock(ctx, addr, MockSpec{Config: map[string]any{"topics": map[string]any{"events": nil}}}, emit)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("mock did not stop")
		}
	}()
	if err := waitKafkaMockReady(addr, time.Second); err != nil {
		t.Fatal(err)
	}
	wire := []byte{0, 128, 255}
	key := []byte{255, 0}
	result, err := p.ExecuteStep(ctx, addr, "publish", map[string]any{"topic": "events", "data": wire, "key": key})
	if err != nil || !result.Success {
		t.Fatalf("publish result=%+v err=%v", result, err)
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ProducerBatchCompression(kgo.GzipCompression()))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	if err := producer.ProduceSync(ctx, &kgo.Record{Topic: "events", Value: []byte("compressed-a")}, &kgo.Record{Topic: "events", Value: []byte("compressed-b")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{addr}, Topic: "events", GroupID: "observed-group", MaxWait: 100 * time.Millisecond, CommitInterval: 0})
	defer reader.Close()
	for i := 0; i < 3; i++ {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (string(msg.Value) != string(wire) || string(msg.Key) != string(key)) {
			t.Fatalf("corrupt binary record: %+v", msg)
		}
		if err := reader.CommitMessages(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	// CommitMessages returns after the broker reply; wait for the observer
	// callback at the end of that write before taking the final snapshot.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		committed := false
		for _, ev := range events {
			if ev.op == "kafka.commit" && ev.fields["offset"] == "3" {
				committed = true
			}
		}
		mu.Unlock()
		if committed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	snapshot := append([]struct {
		op     string
		fields map[string]string
	}(nil), events...)
	mu.Unlock()
	seen := map[string]bool{}
	for _, ev := range snapshot {
		if ev.op == "observation_error" {
			t.Fatalf("observation failed: %v", ev.fields)
		}
		seen[ev.op+":"+ev.fields["offset"]] = true
		if ev.op == "kafka.produce" && ev.fields["offset"] == "0" {
			if ev.fields["value_base64"] != base64.StdEncoding.EncodeToString(wire) {
				t.Fatalf("binary trace: %v", ev.fields)
			}
		}
		if ev.op == "kafka.commit" && ev.fields["group"] != "observed-group" {
			t.Fatalf("group: %v", ev.fields)
		}
	}
	for i := 0; i < 3; i++ {
		for _, op := range []string{"kafka.produce", "kafka.fetch"} {
			if !seen[fmt.Sprintf("%s:%d", op, i)] {
				t.Fatalf("missing %s offset %d: %v", op, i, seen)
			}
		}
	}
	if !seen["kafka.commit:3"] {
		t.Fatalf("missing acknowledged commit: %v", seen)
	}

	for _, explicitKey := range []bool{false, true} {
		topic := fmt.Sprintf("empty-%v", explicitKey)
		args := map[string]any{"topic": topic}
		if explicitKey {
			args["data"] = []byte{}
			args["key"] = []byte{}
		}
		result, err := p.ExecuteStep(ctx, addr, "publish", args)
		if err != nil || !result.Success {
			t.Fatalf("empty publish: %+v %v", result, err)
		}
		r, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: {0: kgo.NewOffset().AtStart()}}))
		if err != nil {
			t.Fatal(err)
		}
		fetched := r.PollRecords(ctx, 1)
		r.Close()
		if err := fetched.Err(); err != nil {
			t.Fatal(err)
		}
		records := fetched.Records()
		if len(records) != 1 {
			t.Fatalf("expected empty record, got %d", len(records))
		}
		msg := records[0]

		if msg.Value == nil || len(msg.Value) != 0 {
			t.Fatalf("empty value became a tombstone: %#v", msg.Value)
		}
		if explicitKey && msg.Key == nil {
			t.Fatal("empty binary key became null")
		}
		if !explicitKey && msg.Key != nil {
			t.Fatal("omitted key became non-null")
		}
	}
}
