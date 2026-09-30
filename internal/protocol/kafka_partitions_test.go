package protocol

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestKafkaInt64PartitionsMetadataAndReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr := freeLoopbackAddr(t)
	events := make(chan kafkaGroupEvent, 1024)
	done := make(chan error, 1)
	go func() {
		done <- (&kafkaProtocol{}).ServeMock(ctx, addr, MockSpec{Config: map[string]any{
			"partitions": int64(2), // Exact shape produced by the Starlark config conversion.
			"topics":     map[string]any{"events": nil},
		}}, func(op string, f map[string]string) { events <- kafkaGroupEvent{op, f} })
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if err := waitKafkaMockReady(addr, time.Second); err != nil {
		t.Fatal(err)
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ClientID("two-partition-sut"), kgo.ConsumerGroup("two-partition-group"), kgo.ConsumeTopics("events"), kgo.FetchMaxWait(30*time.Millisecond), kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := kmsg.NewPtrMetadataRequest()
	topic := "events"
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &topic}}
	resp, err := req.RequestWith(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Topics) != 1 || resp.Topics[0].ErrorCode != 0 || len(resp.Topics[0].Partitions) != 2 {
		t.Fatalf("wanted two declared partitions, metadata=%+v", resp.Topics)
	}
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); client.PollRecords(ctx, 1) }()
	defer func() { cancel(); <-pollDone }()
	ready := map[string]bool{}
	for len(ready) < 2 {
		select {
		case e := <-events:
			if e.op == "observation_error" {
				t.Fatal(e.fields)
			}
			if e.op == "kafka.group_ready" {
				if e.fields["group"] != "two-partition-group" || e.fields["topic"] != "events" || e.fields["offset"] != "0" {
					t.Fatalf("bad readiness: %v", e.fields)
				}
				ready[e.fields["partition"]] = true
			}
		case <-ctx.Done():
			t.Fatalf("not all partitions became ready before first publish: %v", ready)
		}
	}
	if !ready["0"] || !ready["1"] {
		t.Fatalf("wrong partitions: %v", ready)
	}
}

func TestKafkaPartitionsRejectsIntegerOverflow(t *testing.T) {
	overflow := int64(math.MaxInt32) + 1
	counts := []any{overflow, int64(math.MaxInt64)}
	if strconv.IntSize == 64 {
		counts = append(counts, int(overflow))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // A regression that starts a broker must not block this error test.
	for _, count := range counts {
		err := (&kafkaProtocol{}).ServeMock(ctx, "127.0.0.1:0", MockSpec{Config: map[string]any{"partitions": count}}, nil)
		if err == nil || !strings.Contains(err.Error(), "partitions") || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("count=%v error=%v", count, err)
		}
	}
}
