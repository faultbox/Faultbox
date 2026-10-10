package protocol

import (
	"fmt"
	"sort"
	"time"
)

type kafkaFetchDiagnostic struct {
	request, response        uint64
	requestedAt, respondedAt time.Time
	requestState, reason     string
	errorCode                int16
}

func (o *kafkaObserver) fetchDiagnostic(p kafkaPending, pos kafkaFetchPosition) *kafkaFetchDiagnostic {
	if o.fetchDiagnostics == nil {
		o.fetchDiagnostics = make(map[kafkaClientPartition]*kafkaFetchDiagnostic)
	}
	key := kafkaClientPartition{p.client, p.source.Instance, pos.kafkaTopicPartition}
	d := o.fetchDiagnostics[key]
	if d == nil {
		d = &kafkaFetchDiagnostic{}
		o.fetchDiagnostics[key] = d
	}
	return d
}
func (o *kafkaObserver) fetchRequested(p kafkaPending) {
	for _, pos := range p.positions {
		d := o.fetchDiagnostic(p, pos)
		d.request = p.sequence
		d.requestedAt = time.Now()
		d.requestState = "pending"
	}
}

// Called with o.mu held. Preserve the greatest request sequence, even when
// earlier responses arrive later; their timestamps are not a new assignment.
func (o *kafkaObserver) fetchResponded(p kafkaPending, pos kafkaFetchPosition, code int16, reason string) {
	d := o.fetchDiagnostic(p, pos)
	if p.sequence >= d.response {
		d.response = p.sequence
		d.respondedAt = time.Now()
		d.errorCode = code
		d.reason = reason
	}
	if p.sequence == d.request {
		d.requestState = "responded"
	}
}
func (o *kafkaObserver) fetchClosed(p kafkaPending) { o.fetchUnanswered(p, "connection_closed") }
func (o *kafkaObserver) fetchUnanswered(p kafkaPending, state string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, pos := range p.positions {
		d := o.fetchDiagnostic(p, pos)
		if d.request == p.sequence && d.requestState == "pending" {
			d.requestState = state
		}
	}
}

// Snapshot is diagnostic only: it does not mutate readiness, create records or
// pronounce a client/broker stalled. Raw Fetch traffic is not added to the trace.
func (o *kafkaObserver) readinessSnapshot() []map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := []map[string]string{}
	now := time.Now()
	for group, g := range o.groups {
		for member, m := range g.members {
			base := sourceFields(groupFields(group, member, m.client, g), m.source)
			base["assignment"] = fmt.Sprint(m.assignment)
			if !m.assignedAt.IsZero() {
				base["assignment_time"] = m.assignedAt.UTC().Format(time.RFC3339Nano)
				base["assignment_age_ms"] = fmt.Sprint(now.Sub(m.assignedAt).Milliseconds())
			}
			if len(m.partitions) == 0 {
				base["state"] = "awaiting_assignment"
				base["ready"] = "false"
				result = append(result, base)
				continue
			}
			for part, ready := range m.partitions {
				f := make(map[string]string, len(base)+12)
				for k, v := range base {
					f[k] = v
				}
				f["fetch_identity_scope"] = "source_instance+client_id+topic+partition"
				f["topic"], f["partition"], f["ready"] = part.topic, fmt.Sprint(part.partition), fmt.Sprint(ready)
				state := "no_fetch_observed"
				if d := o.fetchDiagnostics[kafkaClientPartition{m.client, m.source.Instance, part}]; d != nil {
					f["last_request_sequence"], f["last_response_sequence"] = fmt.Sprint(d.request), fmt.Sprint(d.response)
					f["last_request_state"] = d.requestState
					if d.response > 0 {
						f["last_response_error"] = fmt.Sprint(d.errorCode)
						f["last_response_disposition"] = d.reason
					}
					if !d.requestedAt.IsZero() {
						f["last_request_time"] = d.requestedAt.UTC().Format(time.RFC3339Nano)
						f["last_request_age_ms"] = fmt.Sprint(now.Sub(d.requestedAt).Milliseconds())
					}
					if !d.respondedAt.IsZero() {
						f["last_response_time"] = d.respondedAt.UTC().Format(time.RFC3339Nano)
					}
					switch {
					case d.request == 0:
						state = "no_fetch_observed"
					case d.request < g.epoch || d.request < m.assignment:
						state = "no_fetch_for_current_assignment"
					case d.requestState == "connection_closed":
						state = "fetch_connection_closed"
					case d.requestState == "response_not_observed":
						state = "response_not_observed"
					case d.requestState == "pending":
						state = "awaiting_fetch_response"
					case d.errorCode != 0:
						state = "fetch_error"
					default:
						state = d.reason
					}
				}
				// A transient process lookup miss is not proof of no traffic.
				// Keep unowned exchanges separate from this member's proven
				// source, but expose them as a possible attribution gap.
				if m.source.Known() {
					own := o.fetchDiagnostics[kafkaClientPartition{m.client, m.source.Instance, part}]
					unowned := o.fetchDiagnostics[kafkaClientPartition{m.client, "", part}]
					if unowned != nil && (own == nil || unowned.request > own.request) {
						f["unattributed_request_sequence"] = fmt.Sprint(unowned.request)
						f["unattributed_request_state"] = unowned.requestState
						f["unattributed_response_disposition"] = unowned.reason
						state = "ownership_unresolved"
					}
				}
				if ready {
					state = "ready"
				}
				if !o.activeSource(m.source) {
					state = "source_inactive"
					f["ready"] = "false"
				}
				f["state"] = state
				result = append(result, f)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		return a["group"]+"\x00"+a["member_id"]+"\x00"+a["topic"]+"\x00"+a["partition"] < b["group"]+"\x00"+b["member_id"]+"\x00"+b["topic"]+"\x00"+b["partition"]
	})
	return result
}
