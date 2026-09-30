package protocol

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestKafkaMissingTopicDiagnosticOnRealMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := freeLoopbackAddr(t)
	var mu sync.Mutex
	var events []kafkaGroupEvent
	done := make(chan error, 1)
	go func() {
		done <- (&kafkaProtocol{}).ServeMock(ctx, addr, MockSpec{}, func(op string, f map[string]string) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, kafkaGroupEvent{op, f})
		})
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
	client, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ClientID("subscriber"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	topic := "absent"
	req := kmsg.NewPtrMetadataRequest()
	req.Topics = []kmsg.MetadataRequestTopic{{Topic: &topic}}
	req.AllowAutoTopicCreation = false
	for i := 0; i < 2; i++ {
		resp, err := req.RequestWith(ctx, client)
		if err != nil || len(resp.Topics) != 1 || resp.Topics[0].ErrorCode != 3 {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	}
	// Flush observation of previous replies by sending another request; the
	// connection's writer serializes response observation before the next reply.
	if _, err := req.RequestWith(ctx, client); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, e := range events {
		if e.op == "kafka.topic_missing" {
			count++
			if e.fields["topic"] != topic || e.fields["client_id"] != "subscriber" {
				t.Fatal(e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("missing-topic diagnostic count=%d", count)
	}
}

func TestKafkaMissingTopicRearmsAfterSuccessfulMetadata(t *testing.T) {
	count := 0
	o := &kafkaObserver{emit: func(op string, f map[string]string) {
		if op == "kafka.topic_missing" {
			count++
		}
	}}
	name := "orders"
	for _, code := range []int16{3, 3, 0, 3, 3} {
		o.observe(kafkaPending{client: "same-client", req: kmsg.NewPtrMetadataRequest()}, &kmsg.MetadataResponse{Topics: []kmsg.MetadataResponseTopic{{Topic: &name, ErrorCode: code}}})
	}
	if count != 2 {
		t.Fatalf("warning count=%d, want one per missing episode", count)
	}
}
