package star

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/faultbox/Faultbox/internal/connowner"
	"go.starlark.net/starlark"
)

// Receipt identity is deliberately not a forgeable JSON dictionary. Numeric
// offsets can be reused by a restarted mock or the next test's topic.
type kafkaReceipt struct {
	log            *EventLog
	generation     uint64
	broker, iface  string
	started, after int64
	Topic          string `json:"topic"`
	Partition      int32  `json:"partition"`
	Offset         int64  `json:"offset"`
}

func (l *EventLog) kafkaScope() (uint64, int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.generation, l.seq
}
func (rt *Runtime) kafkaHistory(ref *InterfaceRef) ([]Event, int64) {
	var result []Event
	var start int64
	for _, e := range rt.events.EventsByService(ref.Service.Name) {
		if e.Fields["interface"] != ref.Interface.Name {
			continue
		}
		if e.Type == "mock.started" {
			result = nil
			start = e.Seq
		}
		if e.Type == "mock.stopped" {
			result = nil
			start = 0
			continue
		}
		result = append(result, e)
	}
	return result, start
}
func fieldInt(f map[string]string, k string) int64 { v, _ := strconv.ParseInt(f[k], 10, 64); return v }
func positionID(f map[string]string) string {
	return strings.Join([]string{f["group"], f["member_id"], f["epoch"], f["generation"], f["assignment"], f["source_instance"], f["topic"], f["partition"]}, "\x00")
}
func sameAssignment(a, b map[string]string) bool {
	for _, key := range []string{"group", "member_id", "epoch", "generation", "assignment", "source_instance"} {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}

// Derive a current snapshot, never an any(historical-ready) predicate. Empty
// assignments, partial SyncGroup emission and unresolved source selectors cannot
// satisfy it. The observer's epoch/assignment tokens order concurrent replies.
func kafkaReadyPositions(events []Event, topics []string, service, group string, active func(map[string]string) bool) ([]map[string]string, string, error) {
	type generation struct{ epoch, number int64 }
	latest := map[string]generation{}
	for _, e := range events {
		f := e.Fields
		if f["group"] == "" || f["epoch"] == "" {
			continue
		}
		g := generation{fieldInt(f, "epoch"), fieldInt(f, "generation")}
		old, ok := latest[f["group"]]
		if !ok || g.epoch > old.epoch || (g.epoch == old.epoch && g.number > old.number) {
			latest[f["group"]] = g
		}
	}
	current := func(f map[string]string) bool {
		g, ok := latest[f["group"]]
		return ok && fieldInt(f, "epoch") == g.epoch && fieldInt(f, "generation") == g.number
	}
	syncs := map[string]map[string]string{}
	for _, e := range events {
		if e.Type != "mock.kafka.group_sync" || !current(e.Fields) {
			continue
		}
		f := e.Fields
		key := f["group"] + "\x00" + f["member_id"]
		if old := syncs[key]; old == nil || fieldInt(f, "assignment") > fieldInt(old, "assignment") {
			syncs[key] = f
		}
	}
	requested := map[string]bool{}
	for _, t := range topics {
		requested[t] = true
	}
	assignments := map[string]map[string]string{}
	groups := map[string]bool{}
	for _, e := range events {
		f := e.Fields
		if e.Type != "mock.kafka.assign" || !current(f) || !requested[f["topic"]] {
			continue
		}
		if group != "" && f["group"] != group {
			continue
		}
		if service != "" && f["source_service"] != service {
			continue
		}
		sync := syncs[f["group"]+"\x00"+f["member_id"]]
		if sync == nil || !sameAssignment(f, sync) {
			continue
		}
		groups[f["group"]] = true
		assignments[positionID(f)] = f
	}
	if len(groups) > 1 {
		names := []string{}
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, "", codedf(CodeKafkaWaitAmbiguous, "Kafka selector matches groups %s; specify group=", strings.Join(names, ", "))
	}
	ready := map[string]bool{}
	for _, e := range events {
		if e.Type == "mock.kafka.group_ready" {
			ready[positionID(e.Fields)] = true
		}
	}
	seen := map[string]bool{}
	positions := []map[string]string{}
	pending := []string{}
	for key, f := range assignments {
		seen[f["topic"]] = true
		if active != nil && !active(f) {
			pending = append(pending, f["topic"]+"/"+f["partition"]+": source process unavailable or ownership unresolved")
			continue
		}
		if !ready[key] {
			pending = append(pending, f["topic"]+"/"+f["partition"]+" awaiting acknowledged Fetch")
			continue
		}
		positions = append(positions, f)
	}
	for _, topic := range topics {
		if !seen[topic] {
			pending = append(pending, topic+": no current assignment for selector")
		}
	}
	if len(pending) > 0 {
		// Include protocol evidence, but do not infer that a subscription alone
		// creates a topic or that lack of an event proves lack of a connection.
		for _, e := range events {
			if e.Type == "mock.kafka.topic_missing" && requested[e.Fields["topic"]] {
				pending = append(pending, e.Fields["topic"]+": broker reported missing topic; declare it in topics=")
			}
		}
		sort.Strings(pending)
		return nil, strings.Join(pending, "; "), nil
	}
	sort.Slice(positions, func(i, j int) bool { return positionID(positions[i]) < positionID(positions[j]) })
	return positions, "", nil
}

func (rt *Runtime) executeKafkaWait(thread *starlark.Thread, ref *InterfaceRef, method string, args starlark.Tuple, kwargs []starlark.Tuple) (value starlark.Value, err error) {
	if !ref.Service.IsMock() {
		return nil, codedf(CodeKafkaWaitUnsupported, "%s currently requires an observed Kafka mock", method)
	}
	if thread != nil && strings.HasPrefix(thread.Name, "monitor:") {
		return nil, fmt.Errorf("%s cannot block a monitor", method)
	}
	var serviceVal starlark.Value = starlark.None
	var group string
	timeout := "10s"
	var topicsVal *starlark.List
	var response *Response
	if method == "wait_ready" {
		err = starlark.UnpackArgs(method, args, kwargs, "topics", &topicsVal, "service?", &serviceVal, "group?", &group, "timeout?", &timeout)
	} else {
		err = starlark.UnpackArgs(method, args, kwargs, "receipt", &response, "service?", &serviceVal, "group?", &group, "timeout?", &timeout)
	}
	if err != nil {
		return nil, err
	}
	service := ""
	switch v := serviceVal.(type) {
	case starlark.NoneType:
	case starlark.String:
		service = string(v)
	case *ServiceDef:
		service = v.Name
	default:
		return nil, fmt.Errorf("%s: service must be a service or name", method)
	}
	if service == "" && group == "" {
		return nil, fmt.Errorf("%s requires service= or group=; selectors are never inferred from client_id", method)
	}
	if service != "" {
		if _, ok := rt.services[service]; !ok {
			return nil, fmt.Errorf("%s: unknown source service %q", method, service)
		}
	}
	duration, parseErr := time.ParseDuration(timeout)
	if parseErr != nil || duration <= 0 {
		return nil, fmt.Errorf("%s: timeout must be a positive duration", method)
	}
	topics := []string{}
	var receipt *kafkaReceipt
	generation, _ := rt.events.kafkaScope()
	if method == "wait_ready" {
		seen := map[string]bool{}
		for i := 0; i < topicsVal.Len(); i++ {
			topic, ok := starlark.AsString(topicsVal.Index(i))
			if !ok || topic == "" {
				return nil, fmt.Errorf("wait_ready: topics must contain non-empty strings")
			}
			if !seen[topic] {
				topics = append(topics, topic)
				seen[topic] = true
			}
		}
		if len(topics) == 0 {
			return nil, fmt.Errorf("wait_ready: topics must not be empty")
		}
	} else {
		receipt = response.kafkaReceipt
		if !response.Ok || receipt == nil || receipt.log != rt.events || receipt.generation != generation || receipt.broker != ref.Service.Name || receipt.iface != ref.Interface.Name {
			return nil, codedf(CodeKafkaReceiptStale, "wait_committed requires a successful publish response from this broker and test")
		}
		topics = []string{receipt.Topic}
	}
	_, started := rt.kafkaHistory(ref)
	if started == 0 {
		return nil, codedf(CodeKafkaWaitUnsupported, "Kafka mock has not started")
	}
	if receipt != nil && receipt.started != started {
		return nil, codedf(CodeKafkaReceiptStale, "publish receipt belongs to a previous broker incarnation")
	}
	active := func(f map[string]string) bool {
		if f["source_instance"] == "" {
			return service == ""
		}
		pid, _ := strconv.Atoi(f["source_pid"])
		return rt.connections != nil && rt.connections.IsActive(connowner.Source{Service: f["source_service"], Instance: f["source_instance"], PID: pid})
	}
	start := time.Now()
	rt.events.MergeClock(ref.Service.Name, "test")
	caller := rt.callerInTest(thread)
	send := map[string]string{"target": ref.Service.Name, "interface": ref.Interface.Name, "protocol": "kafka", "method": method, "source_service": service, "group": group}
	if caller != "" {
		send["spec"] = caller
	}
	rt.events.Emit("step_send", "test", send)
	defer func() {
		rt.events.MergeClock("test", ref.Service.Name)
		rt.vacuity.noteStep(ref.Service.Name, ref.Interface.Name, err == nil)
		fields := map[string]string{"target": ref.Service.Name, "interface": ref.Interface.Name, "protocol": "kafka", "method": method, "duration_ms": fmt.Sprint(time.Since(start).Milliseconds()), "success": fmt.Sprint(err == nil)}
		if caller != "" {
			fields["spec"] = caller
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		rt.events.Emit("step_recv", "test", fields)
	}()
	ctx, cancel := context.WithTimeout(rt.executionContext(thread), duration)
	defer cancel()
	// Wake on semantic evidence, not every Fetch poll. The low-frequency
	// timer also notices a process death that has no broker event.
	wake := make(chan struct{}, 1)
	sub := rt.events.Subscribe([]eventFilter{{key: "service", value: ref.Service.Name}}, func(e Event) error {
		if e.Fields["interface"] != ref.Interface.Name {
			return nil
		}
		switch e.Type {
		case "mock.started", "mock.stopped", "mock.kafka.assign", "mock.kafka.group_sync", "mock.kafka.group_join", "mock.kafka.group_ready", "mock.kafka.group_rebalance", "mock.kafka.group_leave", "mock.kafka.commit", "mock.kafka.topic_missing":
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		return nil
	})
	defer rt.events.Unsubscribe(sub)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var pinned map[string]string
	reason := "waiting for current assignment"
	for {
		if ctx.Err() != nil {
			return nil, codedf(CodeKafkaWaitTimeout, "%s for service=%q group=%q topics=%v: %s (%v)", method, service, group, topics, reason, ctx.Err())
		}
		currentGen, _ := rt.events.kafkaScope()
		if currentGen != generation {
			return nil, codedf(CodeKafkaReceiptStale, "test scope changed during Kafka wait")
		}
		history, currentStart := rt.kafkaHistory(ref)
		if currentStart != started {
			return nil, codedf(CodeKafkaReceiptStale, "broker stopped or restarted during Kafka wait")
		}
		positions, why, snapshotErr := kafkaReadyPositions(history, topics, service, group, active)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		reason = why
		if method == "wait_ready" && len(positions) > 0 && why == "" {
			body, _ := json.Marshal(map[string]any{"ready": true, "positions": positions})
			return &Response{Ok: true, Body: string(body), DurationMs: time.Since(start).Milliseconds()}, nil
		}
		if receipt != nil {
			var current map[string]string
			for _, p := range positions {
				if fieldInt(p, "partition") == int64(receipt.Partition) {
					if current != nil {
						return nil, codedf(CodeKafkaWaitAmbiguous, "multiple current consumers own receipt partition; specify service=")
					}
					current = p
				}
			}
			if pinned == nil {
				before := []Event{}
				for _, e := range history {
					if e.Seq <= receipt.after {
						before = append(before, e)
					}
				}
				prior, _, priorErr := kafkaReadyPositions(before, topics, service, group, nil)
				if priorErr != nil {
					return nil, priorErr
				}
				for _, p := range prior {
					if fieldInt(p, "partition") == int64(receipt.Partition) {
						if pinned != nil {
							return nil, codedf(CodeKafkaWaitAmbiguous, "multiple consumers owned the receipt partition at publish time; specify service=")
						}
						pinned = p
					}
				}
				if pinned == nil {
					return nil, codedf(CodeKafkaWaitUnsupported, "no ready consumer owned the partition when publishing; call wait_ready before publishing")
				}
				if current == nil {
					return nil, codedf(CodeKafkaWaitUnsupported, "no ready consumer owns receipt partition; call wait_ready before publishing (%s)", why)
				}
			}
			if current == nil || positionID(current) != positionID(pinned) {
				return nil, codedf(CodeKafkaReceiptStale, "consumer assignment changed during wait_committed; establish readiness and publish a new receipt")
			}
			reason = fmt.Sprintf("awaiting %s/%s commit > %d from group=%s source_instance=%s (commit is not business processing)", receipt.Topic, pinned["partition"], receipt.Offset, pinned["group"], pinned["source_instance"])
			for _, e := range history {
				f := e.Fields
				if e.Type == "mock.kafka.commit" && e.Seq > receipt.after && f["topic"] == receipt.Topic && fieldInt(f, "partition") == int64(receipt.Partition) && f["group"] == pinned["group"] && f["source_instance"] == pinned["source_instance"] && fieldInt(f, "offset") > receipt.Offset {
					body, _ := json.Marshal(map[string]any{"committed": true, "topic": receipt.Topic, "partition": receipt.Partition, "offset": fieldInt(f, "offset"), "group": f["group"], "source_service": f["source_service"], "source_instance": f["source_instance"]})
					return &Response{Ok: true, Body: string(body), DurationMs: time.Since(start).Milliseconds()}, nil
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-ticker.C:
		}
	}
}
