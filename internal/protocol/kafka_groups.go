package protocol

import (
	"fmt"

	"github.com/twmb/franz-go/pkg/kmsg"
)

// Classic Fetch has no group ID. Attribution is deliberately limited to a
// unique CURRENT assignment for (client ID, topic, partition), never a socket.
// Capture that assignment when the request arrives, then validate it again
// when the broker acknowledges the concrete fetch offset.
type kafkaObservedGroup struct {
	generation int32
	epoch      uint64
	members    map[string]*kafkaObservedMember
}
type kafkaObservedMember struct {
	client     string
	assignment uint64
	partitions map[kafkaTopicPartition]bool // value: starting position acknowledged
}
type kafkaTopicPartition struct {
	topic     string
	partition int32
}
type kafkaClientPartition struct {
	client string
	kafkaTopicPartition
}
type kafkaAssignmentRef struct {
	group, member     string
	generation        int32
	epoch, assignment uint64
}
type kafkaFetchPosition struct {
	kafkaTopicPartition
	offset   int64
	accepted bool
	refs     []kafkaAssignmentRef
}
type kafkaSessionPosition struct {
	offset   int64
	accepted bool
}
type kafkaGroupEvent struct {
	op     string
	fields map[string]string
}

func (o *kafkaObserver) group(name string) *kafkaObservedGroup {
	if o.groups == nil {
		o.groups = make(map[string]*kafkaObservedGroup)
	}
	g := o.groups[name]
	if g == nil {
		g = &kafkaObservedGroup{generation: -1, members: make(map[string]*kafkaObservedMember)}
		o.groups[name] = g
	}
	return g
}
func (o *kafkaObserver) invalidate(group, reason string, sequence uint64) kafkaGroupEvent {
	g := o.group(group)
	g.epoch = sequence
	for _, m := range g.members {
		m.partitions = nil
	}
	return kafkaGroupEvent{"kafka.group_rebalance", map[string]string{"group": group, "generation": fmt.Sprint(g.generation), "epoch": fmt.Sprint(g.epoch), "reason": reason}}
}
func (o *kafkaObserver) request(req kmsg.Request, client string) kafkaPending {
	o.mu.Lock()
	o.sequence++
	p := kafkaPending{req: req, client: client, sequence: o.sequence}
	var events []kafkaGroupEvent
	switch r := req.(type) {
	case *kmsg.JoinGroupRequest:
		events = append(events, o.invalidate(r.Group, "join_requested", p.sequence))
	case *kmsg.FetchRequest:
		positions := make(map[kafkaTopicPartition]kafkaSessionPosition)
		if r.SessionID > 0 && r.SessionEpoch > 0 {
			for key, pos := range o.fetchSessions[r.SessionID] {
				positions[key] = pos
			}
		}
		for _, t := range r.ForgottenTopics {
			name := t.Topic
			if name == "" {
				name = o.topics[t.TopicID]
			}
			for _, part := range t.Partitions {
				delete(positions, kafkaTopicPartition{name, part})
			}
		}
		for _, t := range r.Topics {
			name := t.Topic
			if name == "" {
				name = o.topics[t.TopicID]
			}
			for _, part := range t.Partitions {
				key := kafkaTopicPartition{name, part.Partition}
				positions[key] = kafkaSessionPosition{part.FetchOffset, positions[key].accepted}
			}
		}
		for key, position := range positions {
			pos := kafkaFetchPosition{kafkaTopicPartition: key, offset: position.offset, accepted: position.accepted}
			if key.topic == "" || pos.offset < 0 {
				continue
			}
			for group, g := range o.groups {
				for member, m := range g.members {
					if _, ok := m.partitions[key]; ok && m.client == client {
						pos.refs = append(pos.refs, kafkaAssignmentRef{group, member, g.generation, g.epoch, m.assignment})
					}
				}
			}
			p.positions = append(p.positions, pos)
		}

	}
	o.mu.Unlock()
	for _, e := range events {
		emitWith(o.emit, e.op, e.fields)
	}
	return p
}
func groupFields(group, member, client string, g *kafkaObservedGroup) map[string]string {
	return map[string]string{"group": group, "member_id": member, "client_id": client, "generation": fmt.Sprint(g.generation), "epoch": fmt.Sprint(g.epoch)}
}
func (o *kafkaObserver) observeGroup(p kafkaPending, response kmsg.Response) {
	var events []kafkaGroupEvent
	o.mu.Lock()
	switch r := response.(type) {
	case *kmsg.JoinGroupResponse:
		req := p.req.(*kmsg.JoinGroupRequest)
		if r.ErrorCode == 0 {
			g := o.group(req.Group)
			if r.Generation >= g.generation {
				g.generation = r.Generation
				if g.members[r.MemberID] == nil {
					g.members[r.MemberID] = &kafkaObservedMember{client: p.client}
				}
				events = append(events, kafkaGroupEvent{"kafka.group_join", groupFields(req.Group, r.MemberID, p.client, g)})
			}
		}
	case *kmsg.SyncGroupResponse:
		req := p.req.(*kmsg.SyncGroupRequest)
		g := o.group(req.Group)
		if r.ErrorCode == 0 && req.Generation == g.generation && p.sequence >= g.epoch && (g.members[req.MemberID] == nil || g.members[req.MemberID].assignment <= p.sequence) {
			var assignment kmsg.ConsumerMemberAssignment
			if err := assignment.ReadFrom(r.MemberAssignment); err == nil {
				m := &kafkaObservedMember{client: p.client, assignment: p.sequence, partitions: make(map[kafkaTopicPartition]bool)}
				g.members[req.MemberID] = m
				for _, t := range assignment.Topics {
					for _, part := range t.Partitions {
						m.partitions[kafkaTopicPartition{t.Topic, part}] = false
						f := groupFields(req.Group, req.MemberID, p.client, g)
						f["topic"], f["partition"], f["assignment"] = t.Topic, fmt.Sprint(part), fmt.Sprint(m.assignment)
						events = append(events, kafkaGroupEvent{"kafka.assign", f})
					}
				}
				f := groupFields(req.Group, req.MemberID, p.client, g)
				f["partitions"] = fmt.Sprint(len(m.partitions))
				f["assignment"] = fmt.Sprint(m.assignment)
				events = append(events, kafkaGroupEvent{"kafka.group_sync", f})
			}
		}
	case *kmsg.HeartbeatResponse:
		req := p.req.(*kmsg.HeartbeatRequest)
		g := o.group(req.Group)
		if r.ErrorCode != 0 && req.Generation == g.generation && p.sequence >= g.epoch {
			events = append(events, o.invalidate(req.Group, fmt.Sprintf("heartbeat_error_%d", r.ErrorCode), p.sequence))
		}
	case *kmsg.LeaveGroupResponse:
		req := p.req.(*kmsg.LeaveGroupRequest)
		if r.ErrorCode == 0 {
			g := o.group(req.Group)
			if p.sequence >= g.epoch {
				members := []string{req.MemberID}
				if req.Version >= 3 {
					members = nil
					for _, m := range r.Members {
						if m.ErrorCode == 0 {
							members = append(members, m.MemberID)
						}
					}
				}
				for _, member := range members {
					delete(g.members, member)
					events = append(events, kafkaGroupEvent{"kafka.group_leave", groupFields(req.Group, member, p.client, g)})
				}
				events = append(events, o.invalidate(req.Group, "member_left", p.sequence))
			}
		}
	case *kmsg.FetchResponse:
		events = append(events, o.observeFetch(p, r)...)

	}
	o.mu.Unlock()
	for _, e := range events {
		emitWith(o.emit, e.op, e.fields)
	}
}

// Incremental fetch sessions omit unchanged partitions in both directions.
// Keep the broker-acknowledged position/error state so that a rebalance which
// retains a partition can become ready without publishing a warm-up record.
func (o *kafkaObserver) observeFetch(p kafkaPending, r *kmsg.FetchResponse) []kafkaGroupEvent {
	req := p.req.(*kmsg.FetchRequest)
	if r.ErrorCode != 0 {
		delete(o.fetchSessions, req.SessionID)
		return nil
	}
	statuses := make(map[kafkaTopicPartition]bool)
	for _, t := range r.Topics {
		name := t.Topic
		if name == "" {
			name = o.topics[t.TopicID]
		}
		for _, part := range t.Partitions {
			statuses[kafkaTopicPartition{name, part.Partition}] = part.ErrorCode == 0
		}
	}
	session := make(map[kafkaTopicPartition]kafkaSessionPosition)
	var events []kafkaGroupEvent
	for _, pos := range p.positions {
		accepted, found := statuses[pos.kafkaTopicPartition]
		if !found {
			accepted = pos.accepted && req.SessionID > 0 && req.SessionEpoch > 0
		}
		session[pos.kafkaTopicPartition] = kafkaSessionPosition{pos.offset, accepted}
		if !accepted {
			continue
		}
		events = append(events, kafkaGroupEvent{"kafka.fetch_position", map[string]string{"client_id": p.client, "topic": pos.topic, "partition": fmt.Sprint(pos.partition), "offset": fmt.Sprint(pos.offset), "request_sequence": fmt.Sprint(p.sequence)}})
		matches := 0
		for _, g := range o.groups {
			for _, m := range g.members {
				if _, ok := m.partitions[pos.kafkaTopicPartition]; ok && m.client == p.client {
					matches++
				}
			}
		}
		key := kafkaClientPartition{p.client, pos.kafkaTopicPartition}
		if len(pos.refs) > 1 || matches > 1 {
			if !o.ambiguous[key] {
				if o.ambiguous == nil {
					o.ambiguous = make(map[kafkaClientPartition]bool)
				}
				o.ambiguous[key] = true
				events = append(events, kafkaGroupEvent{"kafka.group_ready_ambiguous", map[string]string{"client_id": p.client, "topic": pos.topic, "partition": fmt.Sprint(pos.partition), "reason": "client_id is shared by multiple active assignments; configure a unique consumer client_id"}})
			}
			continue
		}
		delete(o.ambiguous, key)
		if len(pos.refs) != 1 || matches != 1 {
			continue
		}
		ref := pos.refs[0]
		g := o.groups[ref.group]
		if g == nil || g.epoch != ref.epoch || g.generation != ref.generation {
			continue
		}
		m := g.members[ref.member]
		if m == nil || m.assignment != ref.assignment {
			continue
		}
		if ready, ok := m.partitions[pos.kafkaTopicPartition]; !ok || ready {
			continue
		}
		m.partitions[pos.kafkaTopicPartition] = true
		f := groupFields(ref.group, ref.member, p.client, g)
		f["topic"], f["partition"], f["offset"], f["assignment"] = pos.topic, fmt.Sprint(pos.partition), fmt.Sprint(pos.offset), fmt.Sprint(ref.assignment)
		f["attribution"], f["position_source"] = "unique_client_id", "fetch"
		events = append(events, kafkaGroupEvent{"kafka.group_ready", f})
	}
	if r.SessionID > 0 {
		if o.fetchSessions == nil {
			o.fetchSessions = make(map[int32]map[kafkaTopicPartition]kafkaSessionPosition)
		}
		o.fetchSessions[r.SessionID] = session
	}
	if req.SessionEpoch == -1 {
		delete(o.fetchSessions, req.SessionID)
	}
	return events
}
