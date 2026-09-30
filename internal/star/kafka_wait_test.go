package star

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.starlark.net/starlark"
)

func kafkaTestEvent(kind, topic, partition string, assignment int) Event {
	return Event{Type: "mock.kafka." + kind, Service: "bus", Fields: map[string]string{"interface": "main", "group": "g", "member_id": "m", "client_id": "shared", "epoch": "1", "generation": "1", "assignment": fmt.Sprint(assignment), "topic": topic, "partition": partition}}
}
func TestKafkaReadySnapshotRejectsStaleAndPartialEvidence(t *testing.T) {
	base := []Event{kafkaTestEvent("assign", "orders", "0", 2), kafkaTestEvent("assign", "orders", "1", 2), kafkaTestEvent("group_sync", "", "", 2), kafkaTestEvent("group_ready", "orders", "0", 2)}
	check := func(events []Event, want int) {
		t.Helper()
		got, _, err := kafkaReadyPositions(events, []string{"orders"}, "", "g", nil)
		if err != nil || len(got) != want {
			t.Fatalf("got=%v err=%v want %d", got, err, want)
		}
	}
	check(base, 0)
	base = append(base, kafkaTestEvent("group_ready", "orders", "1", 2))
	check(base, 2)
	stale := kafkaTestEvent("group_rebalance", "", "", 0)
	stale.Fields["epoch"] = "3"
	check(append(base, stale), 0)
	resync := kafkaTestEvent("group_sync", "", "", 4)
	check(append(base, resync), 0)
	missing, why, err := kafkaReadyPositions(base, []string{"absent"}, "", "g", nil)
	if err != nil || len(missing) != 0 || !strings.Contains(why, "no current assignment") {
		t.Fatal(missing, why, err)
	}
	if got, _, _ := kafkaReadyPositions(base, []string{"orders"}, "other", "g", nil); len(got) != 0 {
		t.Fatal("wrong source readied")
	}
	if got, _, _ := kafkaReadyPositions(base, []string{"orders"}, "", "g", func(map[string]string) bool { return false }); len(got) != 0 {
		t.Fatal("dead instance readied")
	}
}
func TestKafkaReadySnapshotRequiresUnambiguousGroup(t *testing.T) {
	events := []Event{}
	for _, group := range []string{"a", "b"} {
		for _, kind := range []string{"assign", "group_sync", "group_ready"} {
			e := kafkaTestEvent(kind, "orders", "0", 2)
			e.Fields["group"] = group
			e.Fields["source_service"] = "consumer"
			events = append(events, e)
		}
	}
	if _, _, err := kafkaReadyPositions(events, []string{"orders"}, "consumer", "", nil); err == nil {
		t.Fatal("ambiguous groups accepted")
	}
	if got, _, err := kafkaReadyPositions(events, []string{"orders"}, "consumer", "b", nil); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
}
func kafkaWaitFixture(t *testing.T) (*Runtime, *InterfaceRef, *Response) {
	t.Helper()
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	if err := rt.LoadString("barriers.star", `bus=mock_service("bus",interface("main","kafka",19092),config={"topics":{"orders":[]}})`); err != nil {
		t.Fatal(err)
	}
	ref := &InterfaceRef{Service: rt.services["bus"], Interface: rt.services["bus"].Interfaces["main"], runtime: rt}
	rt.events.Emit("mock.started", "bus", map[string]string{"interface": "main"})
	for _, kind := range []string{"assign", "group_sync", "group_ready"} {
		e := kafkaTestEvent(kind, "orders", "0", 2)
		rt.events.Emit(e.Type, e.Service, e.Fields)
	}
	generation, seq := rt.events.kafkaScope()
	_, start := rt.kafkaHistory(ref)
	receipt := &Response{Ok: true, kafkaReceipt: &kafkaReceipt{log: rt.events, generation: generation, broker: "bus", iface: "main", started: start, after: seq, Topic: "orders", Partition: 0, Offset: 0}}
	return rt, ref, receipt
}
func waitKwargs() []starlark.Tuple {
	return []starlark.Tuple{{starlark.String("group"), starlark.String("g")}, {starlark.String("timeout"), starlark.String("30ms")}}
}
func TestKafkaCommitBarrierIdentityAndOffset(t *testing.T) {
	for _, kind := range []string{"correct", "wrong_group", "equal_offset", "old_commit", "old_test", "new_broker", "rebalance", "fabricated", "cancelled", "wrong_broker", "wrong_interface", "failed_publish", "large_offset"} {
		t.Run(kind, func(t *testing.T) {
			rt, ref, receipt := kafkaWaitFixture(t)
			e := kafkaTestEvent("commit", "orders", "0", 2)
			e.Fields["offset"] = "1"
			if kind == "large_offset" {
				receipt.kafkaReceipt.Offset = 9007199254740993
				e.Fields["offset"] = "9007199254740994"
			}
			if kind == "wrong_broker" {
				receipt.kafkaReceipt.broker = "other"
			}
			if kind == "wrong_interface" {
				receipt.kafkaReceipt.iface = "other"
			}
			if kind == "failed_publish" {
				receipt.Ok = false
			}
			if kind == "wrong_group" {
				e.Fields["group"] = "other"
			}
			if kind == "equal_offset" {
				e.Fields["offset"] = "0"
			}
			rt.events.Emit(e.Type, e.Service, e.Fields)
			if kind == "old_commit" {
				_, seq := rt.events.kafkaScope()
				receipt.kafkaReceipt.after = seq
			}
			if kind == "old_test" {
				rt.events.Reset()
			}
			if kind == "new_broker" {
				rt.events.Emit("mock.started", "bus", map[string]string{"interface": "main"})
			}
			if kind == "rebalance" {
				e := kafkaTestEvent("group_rebalance", "", "", 0)
				e.Fields["epoch"] = "9"
				rt.events.Emit(e.Type, e.Service, e.Fields)
			}
			if kind == "fabricated" {
				receipt = &Response{Ok: true, Body: `{"published":true,"topic":"orders","partition":0,"offset":0}`}
			}
			thread := &starlark.Thread{Name: "test"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			thread.SetLocal("faultbox.context", ctx)
			if kind == "cancelled" {
				cancel()
			}
			result, err := rt.executeKafkaWait(thread, ref, "wait_committed", starlark.Tuple{receipt}, waitKwargs())
			if kind == "correct" || kind == "large_offset" {
				if err != nil || !result.(*Response).Ok {
					t.Fatal(result, err)
				}
				commits := rt.events.EventsByType("mock.kafka.commit")
				replies := rt.events.EventsByType("step_recv")
				if len(replies) != 1 || replies[0].VectorClock["bus"] < commits[0].VectorClock["bus"] {
					t.Fatal("barrier response did not inherit observed broker evidence")
				}

			} else if err == nil {
				t.Fatalf("%s falsely succeeded", kind)
			}
		})
	}
}

// The exact fenced documentation helper is executed against a real Kafka
// consumer. No synthetic readiness or commit events are supplied.
func TestKafkaBarrierDocumentationExample(t *testing.T) {
	doc, err := os.ReadFile("../../docs/protocols/kafka.md")
	if err != nil {
		t.Fatal(err)
	}
	_, tail, ok := strings.Cut(string(doc), "<!-- executable:kafka-barriers -->")
	if !ok {
		t.Fatal("documented example marker missing")
	}
	_, tail, ok = strings.Cut(tail, "```python\n")
	if !ok {
		t.Fatal("code block missing")
	}
	example, _, ok := strings.Cut(tail, "\n```")
	if !ok {
		t.Fatal("code block unterminated")
	}
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	port := freePort(t)
	if err := rt.LoadString("documented-kafka.star", fmt.Sprintf(`bus=mock_service("bus",interface("main","kafka",%d),config={"topics":{"orders":[]}})
`, port)+example); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.startMockService(ctx, "bus", rt.services["bus"]); err != nil {
		t.Fatal(err)
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(fmt.Sprintf("127.0.0.1:%d", port)), kgo.ConsumerGroup("orders-worker"), kgo.ConsumeTopics("orders"), kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()), kgo.DisableAutoCommit(), kgo.FetchMaxWait(40*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		records := client.PollRecords(ctx, 1)
		if err := records.Err(); err != nil {
			done <- err
			return
		}
		r := records.Records()
		if len(r) != 1 || r[0].Offset != 0 || string(r[0].Value) != "first" {
			done <- fmt.Errorf("first record mismatch: %v", r)
			return
		}
		done <- client.CommitRecords(ctx, r...)
	}()
	thread := &starlark.Thread{Name: "test"}
	thread.SetLocal("faultbox.context", ctx)
	if _, err := starlark.Call(thread, rt.globals["verify_delivery"], starlark.Tuple{rt.globals["bus"]}, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
