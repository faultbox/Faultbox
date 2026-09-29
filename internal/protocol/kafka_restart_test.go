package protocol

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// G4: a successful step must publish to the current broker, even when a
// previous test used the same endpoint. A fresh reader checks the actual
// record, not just the produce acknowledgement.
func TestKafkaPublishAcrossMockRestarts(t *testing.T) {
	p := &kafkaProtocol{}
	addr := freeLoopbackAddr(t)
	for generation := 0; generation < 5; generation++ {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- p.ServeMock(ctx, addr, MockSpec{Config: map[string]any{
					"topics": map[string]any{"events": nil},
				}}, nil)
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
			callCtx, callCancel := context.WithTimeout(ctx, 3*time.Second)
			defer callCancel()
			want := fmt.Sprintf("generation-%d", generation)
			result, err := p.ExecuteStep(callCtx, addr, "publish", map[string]any{"topic": "events", "data": want})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Success {
				t.Fatalf("publish: %s", result.Error)
			}
			reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{addr}, Topic: "events", Partition: 0, MaxWait: 100 * time.Millisecond})
			defer reader.Close()
			msg, err := reader.ReadMessage(callCtx)
			if err != nil {
				t.Fatalf("acked publish was not readable: %v", err)
			}
			if string(msg.Value) != want || msg.Offset != 0 {
				t.Fatalf("got value=%q offset=%d, want %q at offset 0", msg.Value, msg.Offset, want)
			}
		})
	}
}
