package protocol

import (
	"github.com/faultbox/Faultbox/internal/connowner"
	"github.com/twmb/franz-go/pkg/kmsg"
	"testing"
)

func TestKafkaDiagnosticsExplainStaleFetchWithoutReadyingNewAssignment(t *testing.T) {
	o := &kafkaObserver{}
	assign := func(gen int32) {
		j := kmsg.NewPtrJoinGroupRequest()
		j.Group = "g"
		p := o.request(j, "c")
		jr := kmsg.NewPtrJoinGroupResponse()
		jr.Generation = gen
		jr.MemberID = "m"
		o.observe(p, jr)
		s := kmsg.NewPtrSyncGroupRequest()
		s.Group = "g"
		s.Generation = gen
		s.MemberID = "m"
		p = o.request(s, "c")
		sr := kmsg.NewPtrSyncGroupResponse()
		sr.MemberAssignment = (&kmsg.ConsumerMemberAssignment{Topics: []kmsg.ConsumerMemberAssignmentTopic{{Topic: "events", Partitions: []int32{0}}}}).AppendTo(nil)
		o.observe(p, sr)
	}
	fetch := func() kafkaPending {
		r := kmsg.NewPtrFetchRequest()
		r.Topics = []kmsg.FetchRequestTopic{{Topic: "events", Partitions: []kmsg.FetchRequestTopicPartition{{Partition: 0, FetchOffset: 0}}}}
		return o.request(r, "c")
	}
	reply := func(p kafkaPending, code int16) {
		r := kmsg.NewPtrFetchResponse()
		r.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0, ErrorCode: code}}}}
		o.observe(p, r)
	}
	state := func(want string) {
		t.Helper()
		rows := o.readinessSnapshot()
		if len(rows) != 1 || rows[0]["state"] != want {
			t.Fatalf("want %s got %v", want, rows)
		}
		if want != "ready" && rows[0]["ready"] != "false" {
			t.Fatal("diagnostic fabricated readiness")
		}
	}
	assign(1)
	state("no_fetch_observed")
	old := fetch()
	state("awaiting_fetch_response")
	assign(2)
	reply(old, 0)
	state("no_fetch_for_current_assignment")
	if o.readinessSnapshot()[0]["last_response_disposition"] != "stale_epoch_or_generation" {
		t.Fatal(o.readinessSnapshot())
	}
	pending := fetch()
	state("awaiting_fetch_response")
	o.fetchClosed(pending)
	state("fetch_connection_closed")
	reply(fetch(), 6)
	state("fetch_error")
	reply(fetch(), 0)
	state("ready")
}

func TestKafkaDiagnosticsSeparatesUnattributedFetch(t *testing.T) {
	source := connowner.Source{Service: "worker", Instance: "managed", PID: 7}
	o := &kafkaObserver{groups: map[string]*kafkaObservedGroup{"g": {generation: 1, epoch: 1, members: map[string]*kafkaObservedMember{"m": {client: "shared", source: source, assignment: 2, partitions: map[kafkaTopicPartition]bool{{"events", 0}: false}}}}}, sequence: 2}
	req := kmsg.NewPtrFetchRequest()
	req.Topics = []kmsg.FetchRequestTopic{{Topic: "events", Partitions: []kmsg.FetchRequestTopicPartition{{Partition: 0, FetchOffset: 0}}}}
	p := o.request(req, "shared") // process lookup unknown; never borrow member identity
	reply := kmsg.NewPtrFetchResponse()
	reply.Topics = []kmsg.FetchResponseTopic{{Topic: "events", Partitions: []kmsg.FetchResponseTopicPartition{{Partition: 0}}}}
	o.observe(p, reply)
	rows := o.readinessSnapshot()
	if len(rows) != 1 || rows[0]["state"] != "ownership_unresolved" || rows[0]["ready"] != "false" || rows[0]["unattributed_request_sequence"] != "3" || rows[0]["last_request_sequence"] != "" {
		t.Fatalf("unowned fetch was hidden or attributed: %v", rows)
	}
}
