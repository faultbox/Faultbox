package protocol

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestKafkaGroupReadyBeforeFirstPublish(t *testing.T) {
	for _, kind := range []string{"franz-go", "kafka-go"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			addr := freeLoopbackAddr(t)
			events := make(chan kafkaGroupEvent, 1024)
			done := make(chan error, 1)
			p := &kafkaProtocol{}
			go func() {
				done <- p.ServeMock(ctx, addr, MockSpec{Config: map[string]any{"topics": map[string]any{"events": nil}}}, func(op string, f map[string]string) { events <- kafkaGroupEvent{op, f} })
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
			received := make(chan string, 1)
			if kind == "franz-go" {
				c, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ClientID("sut-client"), kgo.ConsumerGroup("sut-group"), kgo.ConsumeTopics("events"), kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()), kgo.FetchMaxWait(50*time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				go func() {
					f := c.PollRecords(ctx, 1)
					if err := f.Err(); err != nil {
						received <- err.Error()
						return
					}
					received <- string(f.Records()[0].Value)
				}()
			} else {
				c := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{addr}, Dialer: &kafka.Dialer{ClientID: "sut-client"}, GroupID: "sut-group", Topic: "events", StartOffset: kafka.LastOffset, MaxWait: 50 * time.Millisecond})
				defer c.Close()
				go func() {
					m, err := c.ReadMessage(ctx)
					if err != nil {
						received <- err.Error()
						return
					}
					received <- string(m.Value)
				}()
			}
			joined, assigned := false, false
			for ready := false; !ready; {
				select {
				case e := <-events:
					if e.op == "observation_error" {
						t.Fatal(e.fields)
					}
					if e.op == "kafka.group_join" && e.fields["group"] == "sut-group" {
						joined = true
					}
					if e.op == "kafka.assign" && e.fields["group"] == "sut-group" {
						assigned = true
					}
					if e.op == "kafka.group_ready" {
						if !joined || !assigned {
							t.Fatalf("ready without join and assign: %v", e)
						}
						if e.fields["group"] != "sut-group" || e.fields["offset"] != "0" || e.fields["attribution"] != "unique_client_id" {
							t.Fatalf("bad readiness: %v", e)
						}
						ready = true
					}
				case <-ctx.Done():
					t.Fatal("consumer did not become ready before publish")
				}
			}
			r, err := p.ExecuteStep(ctx, addr, "publish", map[string]any{"topic": "events", "data": "first-real-record"})
			if err != nil || !r.Success {
				t.Fatalf("publish: %+v %v", r, err)
			}
			select {
			case got := <-received:
				if got != "first-real-record" {
					t.Fatal(got)
				}
			case <-ctx.Done():
				t.Fatal("first publish skipped")
			}
		})
	}
}

func TestKafkaGroupReadyRejectsStaleAndAmbiguousAssignments(t *testing.T) {
	var mu sync.Mutex
	var events []kafkaGroupEvent
	o := &kafkaObserver{topics: map[[16]byte]string{}, emit: func(op string, f map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, kafkaGroupEvent{op, f})
	}}
	assign := func(group, member string, generation int32, topics ...string) {
		topic := "events"
		if len(topics) > 0 {
			topic = topics[0]
		}
		join := kmsg.NewPtrJoinGroupRequest()
		join.Group = group
		p := o.request(join, "shared-client")
		r := kmsg.NewPtrJoinGroupResponse()
		r.Generation = generation
		r.MemberID = member
		o.observe(p, r)
		sync := kmsg.NewPtrSyncGroupRequest()
		sync.Group = group
		sync.Generation = generation
		sync.MemberID = member
		p = o.request(sync, "shared-client")
		sr := kmsg.NewPtrSyncGroupResponse()
		sr.MemberAssignment = (&kmsg.ConsumerMemberAssignment{Topics: []kmsg.ConsumerMemberAssignmentTopic{{Topic: topic, Partitions: []int32{0}}}}).AppendTo(nil)
		o.observe(p, sr)
	}
	fetch := func() kafkaPending {
		req := kmsg.NewPtrFetchRequest()
		req.Topics = []kmsg.FetchRequestTopic{{Topic: "events", Partitions: []kmsg.FetchRequestTopicPartition{{Partition: 0, FetchOffset: 42}}}}
		return o.request(req, "shared-client")
	}
	reply := func(p kafkaPending, code int16) {
		r := kmsg.NewPtrFetchResponse()
		r.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0, ErrorCode: code}}}}
		o.observe(p, r)
	}
	count := func(op string) int {
		n := 0
		for _, e := range events {
			if e.op == op {
				n++
			}
		}
		return n
	}
	assign("one", "m1", 1)
	stale := fetch()
	assign("one", "m1", 2)
	reply(stale, 0)
	if count("kafka.group_ready") != 0 {
		t.Fatal("old response marked new generation ready")
	}
	reply(fetch(), 1)
	if count("kafka.group_ready") != 0 {
		t.Fatal("failed fetch marked ready")
	}
	beforeOverlap := fetch()
	assign("two", "m2", 1)
	reply(beforeOverlap, 0)
	reply(fetch(), 0)
	if count("kafka.group_ready") != 0 || count("kafka.group_ready_ambiguous") != 1 {
		t.Fatal("ambiguous client ID must not be attributed")
	}
	leave := kmsg.NewPtrLeaveGroupRequest()
	leave.Group = "two"
	leave.MemberID = "m2"
	o.observe(o.request(leave, "shared-client"), kmsg.NewPtrLeaveGroupResponse())
	reply(fetch(), 0)
	if count("kafka.group_ready") != 1 {
		t.Fatalf("wanted one ready after ambiguity cleared: %v", events)
	}
	reply(fetch(), 0)
	if count("kafka.group_ready") != 1 {
		t.Fatal("repeated ready for same assignment")
	}

	// An accepted incremental fetch may omit unchanged empty partitions. A
	// retained assignment must still become ready without publishing a marker.
	assign("one", "m1", 3)
	first := fetch()
	first.req.(*kmsg.FetchRequest).SessionEpoch = 0
	sessionReply := kmsg.NewPtrFetchResponse()
	sessionReply.SessionID = 77
	sessionReply.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0}}}}
	o.observe(first, sessionReply)
	assign("one", "m1", 4)
	incremental := kmsg.NewPtrFetchRequest()
	incremental.SessionID, incremental.SessionEpoch = 77, 1
	emptyReply := kmsg.NewPtrFetchResponse()
	emptyReply.SessionID = 77
	o.observe(o.request(incremental, "shared-client"), emptyReply)
	if count("kafka.group_ready") != 3 {
		t.Fatalf("incremental retained assignment not ready: %v", events)
	}
	assign("one", "m1", 5)
	incremental.SessionEpoch = 2
	incremental.ForgottenTopics = []kmsg.FetchRequestForgottenTopic{{Topic: "events", Partitions: []int32{0}}}
	o.observe(o.request(incremental, "shared-client"), emptyReply)
	if count("kafka.group_ready") != 3 {
		t.Fatal("forgotten partition marked ready")
	}

	assign("two", "m2", 2, "different-topic")
	reply(fetch(), 0)
	if count("kafka.group_ready") != 4 {
		t.Fatal("disjoint assignment with shared client ID blocked readiness")
	}
	for _, e := range events {
		if e.op == "kafka.group_ready" && (e.fields["offset"] != "42") {
			t.Fatal(fmt.Sprint(e))
		}
	}
}
