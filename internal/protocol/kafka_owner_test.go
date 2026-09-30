package protocol

import (
	"testing"

	"github.com/faultbox/Faultbox/internal/connowner"
	"github.com/twmb/franz-go/pkg/kmsg"
)

type kafkaOwnershipFixture struct {
	observer *kafkaObserver
	events   []kafkaGroupEvent
	active   map[string]bool
}

func newKafkaOwnershipFixture() *kafkaOwnershipFixture {
	f := &kafkaOwnershipFixture{active: make(map[string]bool)}
	f.observer = &kafkaObserver{topics: map[[16]byte]string{}, sourceActive: func(s connowner.Source) bool { return f.active[s.Instance] }, emit: func(op string, fields map[string]string) { f.events = append(f.events, kafkaGroupEvent{op, fields}) }}
	return f
}
func (f *kafkaOwnershipFixture) assign(group string, source connowner.Source, generation int32) {
	if source.Known() {
		f.active[source.Instance] = true
	}
	join := kmsg.NewPtrJoinGroupRequest()
	join.Group = group
	jr := kmsg.NewPtrJoinGroupResponse()
	jr.Generation = generation
	jr.MemberID = "member-" + group
	f.observer.observe(f.observer.request(join, "same-client", source), jr)
	sync := kmsg.NewPtrSyncGroupRequest()
	sync.Group = group
	sync.MemberID = jr.MemberID
	sync.Generation = generation
	sr := kmsg.NewPtrSyncGroupResponse()
	sr.MemberAssignment = (&kmsg.ConsumerMemberAssignment{Topics: []kmsg.ConsumerMemberAssignmentTopic{{Topic: "events", Partitions: []int32{0}}}}).AppendTo(nil)
	f.observer.observe(f.observer.request(sync, "same-client", source), sr)
}
func (f *kafkaOwnershipFixture) fetch(source connowner.Source) kafkaPending {
	req := kmsg.NewPtrFetchRequest()
	req.Topics = []kmsg.FetchRequestTopic{{Topic: "events", Partitions: []kmsg.FetchRequestTopicPartition{{Partition: 0, FetchOffset: 0}}}}
	return f.observer.request(req, "same-client", source)
}
func (f *kafkaOwnershipFixture) reply(p kafkaPending) {
	r := kmsg.NewPtrFetchResponse()
	r.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0}}}}
	f.observer.observe(p, r)
}
func (f *kafkaOwnershipFixture) matching(op string) []kafkaGroupEvent {
	var found []kafkaGroupEvent
	for _, e := range f.events {
		if e.op == op {
			found = append(found, e)
		}
	}
	return found
}

func TestKafkaReadinessUsesEachProcessesOwnFetch(t *testing.T) {
	f := newKafkaOwnershipFixture()
	a := connowner.Source{Service: "same-service", Instance: "launch-A", PID: 100}
	b := connowner.Source{Service: "same-service", Instance: "launch-B", PID: 200}
	f.assign("group-a", a, 1)
	f.assign("group-b", b, 1)
	f.reply(f.fetch(a))
	ready := f.matching("kafka.group_ready")
	if len(ready) != 1 || ready[0].fields["group"] != "group-a" {
		t.Fatalf("A's Fetch must not make B ready: %v", ready)
	}
	if ready[0].fields["source_instance"] != "launch-A" || ready[0].fields["source_service"] != "same-service" || ready[0].fields["source_pid"] != "100" || ready[0].fields["attribution"] != "process_instance" {
		t.Fatalf("missing process evidence: %v", ready)
	}
	f.reply(f.fetch(b))
	ready = f.matching("kafka.group_ready")
	if len(ready) != 2 || ready[1].fields["group"] != "group-b" || ready[1].fields["source_instance"] != "launch-B" {
		t.Fatalf("B needs its own Fetch: %v", ready)
	}
}

func TestKafkaOwnershipUncertaintyNeverBorrowsManagedReadiness(t *testing.T) {
	known := connowner.Source{Service: "sut", Instance: "launch", PID: 100}
	unknown := connowner.Source{}
	for _, tc := range []struct {
		name              string
		assignment, fetch connowner.Source
		extra             *connowner.Source
		diagnostic        string
	}{
		{"unknown fetch", known, unknown, nil, "kafka.group_attribution_unavailable"},
		{"unknown assignment", unknown, known, nil, "kafka.group_attribution_unavailable"},
		{"unresolved competing assignment", known, known, &unknown, "kafka.group_ready_ambiguous"},
		{"two groups same process", known, known, &known, "kafka.group_ready_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKafkaOwnershipFixture()
			f.active[known.Instance] = true
			f.assign("group-a", tc.assignment, 1)
			if tc.extra != nil {
				f.assign("group-b", *tc.extra, 1)
			}
			f.reply(f.fetch(tc.fetch))
			if ready := f.matching("kafka.group_ready"); len(ready) != 0 {
				t.Fatalf("borrowed readiness: %v", ready)
			}
			if len(f.matching(tc.diagnostic)) == 0 {
				t.Fatalf("missing honest diagnostic: %v", f.events)
			}
		})
	}
	f := newKafkaOwnershipFixture()
	f.assign("external", unknown, 1)
	f.reply(f.fetch(unknown))
	ready := f.matching("kafka.group_ready")
	if len(ready) != 1 || ready[0].fields["attribution"] != "unique_client_id" {
		t.Fatalf("external compatibility: %v", ready)
	}
}

func TestKafkaOwnershipRejectsPIDReuseAndStaleResponses(t *testing.T) {
	f := newKafkaOwnershipFixture()
	old := connowner.Source{Service: "sut", Instance: "old-launch", PID: 100}
	fresh := connowner.Source{Service: "sut", Instance: "new-launch", PID: 100}
	f.assign("group", old, 1)
	pending := f.fetch(old)
	f.active[old.Instance] = false
	f.assign("group", fresh, 2)
	f.reply(pending)
	if len(f.matching("kafka.group_ready")) != 0 {
		t.Fatal("old process response marked new process ready")
	}
	f.reply(f.fetch(fresh))
	ready := f.matching("kafka.group_ready")
	if len(ready) != 1 || ready[0].fields["source_instance"] != fresh.Instance {
		t.Fatalf("fresh launch readiness: %v", ready)
	}
}

func TestKafkaFetchSessionsAreScopedToProcessInstance(t *testing.T) {
	f := newKafkaOwnershipFixture()
	a := connowner.Source{Service: "sut-a", Instance: "A", PID: 100}
	b := connowner.Source{Service: "sut-b", Instance: "B", PID: 200}
	f.assign("group-a", a, 1)
	first := f.fetch(a)
	first.req.(*kmsg.FetchRequest).SessionEpoch = 0
	reply := kmsg.NewPtrFetchResponse()
	reply.SessionID = 77
	reply.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0}}}}
	f.observer.observe(first, reply)
	f.assign("group-b", b, 1)
	incremental := kmsg.NewPtrFetchRequest()
	incremental.SessionID = 77
	incremental.SessionEpoch = 1
	empty := kmsg.NewPtrFetchResponse()
	empty.SessionID = 77
	f.observer.observe(f.observer.request(incremental, "same-client", b), empty)
	ready := f.matching("kafka.group_ready")
	if len(ready) != 1 || ready[0].fields["group"] != "group-a" {
		t.Fatalf("B borrowed A's fetch session: %v", ready)
	}
	f.reply(f.fetch(b))
	if len(f.matching("kafka.group_ready")) != 2 {
		t.Fatal("own Fetch did not position B")
	}
}

func TestKafkaCommitEvidenceCarriesRequestOwner(t *testing.T) {
	f := newKafkaOwnershipFixture()
	owner := connowner.Source{Service: "worker", Instance: "worker-launch", PID: 42}
	f.active[owner.Instance] = true
	req := kmsg.NewPtrOffsetCommitRequest()
	req.Group = "group"
	req.Topics = []kmsg.OffsetCommitRequestTopic{{Topic: "events", Partitions: []kmsg.OffsetCommitRequestTopicPartition{{Partition: 0, Offset: 10}}}}
	resp := kmsg.NewPtrOffsetCommitResponse()
	resp.Topics = []kmsg.OffsetCommitResponseTopic{{Topic: "events", Partitions: []kmsg.OffsetCommitResponseTopicPartition{{Partition: 0}}}}
	f.observer.observe(f.observer.request(req, "same-client", owner), resp)
	commits := f.matching("kafka.commit")
	if len(commits) != 1 || commits[0].fields["source_instance"] != owner.Instance || commits[0].fields["source_service"] != owner.Service || commits[0].fields["source_pid"] != "42" {
		t.Fatalf("commit source was lost: %v", commits)
	}
}

func TestKafkaRecordEvidenceCarriesRequestOwner(t *testing.T) {
	f := newKafkaOwnershipFixture()
	owner := connowner.Source{Service: "worker", Instance: "worker-launch", PID: 42}
	f.active[owner.Instance] = true
	record := kmsg.Record{Value: []byte("payload")}
	record.Length = int32(len(record.AppendTo(nil)) - 1)
	batch := kmsg.RecordBatch{Magic: 2, NumRecords: 1, Records: record.AppendTo(nil)}
	batch.Length = int32(len(batch.AppendTo(nil)) - 12)
	produce := kmsg.NewPtrProduceRequest()
	produce.Topics = []kmsg.ProduceRequestTopic{{Topic: "events", Partitions: []kmsg.ProduceRequestTopicPartition{{Partition: 0, Records: batch.AppendTo(nil)}}}}
	produced := kmsg.NewPtrProduceResponse()
	produced.Topics = []kmsg.ProduceResponseTopic{{Topic: "events", Partitions: []kmsg.ProduceResponseTopicPartition{{Partition: 0, BaseOffset: 9}}}}
	f.observer.observe(f.observer.request(produce, "same-client", owner), produced)
	fetch := kmsg.NewPtrFetchRequest()
	fetch.Topics = []kmsg.FetchRequestTopic{{Topic: "events", Partitions: []kmsg.FetchRequestTopicPartition{{Partition: 0, FetchOffset: 9}}}}
	batch.FirstOffset = 9
	fetched := kmsg.NewPtrFetchResponse()
	fetched.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0, RecordBatches: batch.AppendTo(nil)}}}}
	f.observer.observe(f.observer.request(fetch, "same-client", owner), fetched)
	for _, op := range []string{"kafka.produce", "kafka.fetch"} {
		events := f.matching(op)
		if len(events) != 1 || events[0].fields["source_instance"] != owner.Instance || events[0].fields["source_service"] != owner.Service || events[0].fields["source_pid"] != "42" || events[0].fields["offset"] != "9" {
			t.Fatalf("%s lost request source: %v", op, events)
		}
	}
}
