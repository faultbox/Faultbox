package star

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is a single entry in the event log.
// Fields are PObserve-compatible: PartitionKey routes to a monitor instance,
// EventType uses dotted notation (e.g., "syscall.write", "lifecycle.started").
type Event struct {
	Seq          int64             `json:"seq"`
	Timestamp    time.Time         `json:"timestamp"`
	Type         string            `json:"type"`                    // short type: "syscall", "lifecycle", etc.
	EventType    string            `json:"event_type"`              // PObserve-compatible dotted type
	PartitionKey string            `json:"partition_key,omitempty"` // PObserve-compatible partition key
	Service      string            `json:"service,omitempty"`
	Fields       map[string]string `json:"fields,omitempty"`
	VectorClock  map[string]int64  `json:"vector_clock,omitempty"` // ShiViz-compatible vector clock
}

// Subscriber receives events as they are emitted.
type Subscriber struct {
	ID      int
	Filters []eventFilter
	OnEvent func(Event) error
}

// EventLog is a thread-safe, append-only ordered event log with vector clocks.
type EventLog struct {
	mu     sync.RWMutex
	events []Event
	seq    int64
	clocks map[string]map[string]int64 // per-service vector clocks

	// Secondary indexes for O(log N) event lookups (RFC-041 §8.5).
	// byType and byService map to slices of indices into events[].
	// Built incrementally in Emit; reset in Reset.
	byType    map[string][]int
	byService map[string][]int

	// Subscribers notified on each Emit.
	subMu       sync.RWMutex
	subscribers []Subscriber
	nextSubID   int
}

// NewEventLog creates a new empty event log.
func NewEventLog() *EventLog {
	return &EventLog{
		clocks:    make(map[string]map[string]int64),
		byType:    make(map[string][]int),
		byService: make(map[string][]int),
	}
}

// Emit appends an event to the log with automatic vector clock tracking.
func (l *EventLog) Emit(typ, service string, fields map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++

	// Advance vector clock for this service.
	if service != "" {
		if l.clocks[service] == nil {
			l.clocks[service] = make(map[string]int64)
		}
		l.clocks[service][service]++
	}

	// Snapshot the current vector clock.
	var vc map[string]int64
	if service != "" && l.clocks[service] != nil {
		vc = make(map[string]int64, len(l.clocks[service]))
		for k, v := range l.clocks[service] {
			vc[k] = v
		}
	}

	// Build PObserve-compatible event type.
	eventType := typ
	if syscall, ok := fields["syscall"]; ok {
		eventType = typ + "." + syscall
	} else if typ == "service_started" || typ == "service_ready" {
		eventType = "lifecycle." + strings.TrimPrefix(typ, "service_")
	} else if typ == "step_send" || typ == "step_recv" {
		if target, ok := fields["target"]; ok {
			eventType = typ + "." + target
		}
	}

	ev := Event{
		Seq:          l.seq,
		Timestamp:    time.Now(),
		Type:         typ,
		EventType:    eventType,
		PartitionKey: service, // default partition = service name
		Service:      service,
		Fields:       fields,
		VectorClock:  vc,
	}
	idx := len(l.events)
	l.events = append(l.events, ev)
	if ev.Type != "" {
		l.byType[ev.Type] = append(l.byType[ev.Type], idx)
	}
	if ev.Service != "" {
		l.byService[ev.Service] = append(l.byService[ev.Service], idx)
	}

	// Notify subscribers (under a separate lock to avoid deadlock).
	// Copy subscriber list under read lock, then call outside the event lock.
	l.subMu.RLock()
	subs := make([]Subscriber, len(l.subscribers))
	copy(subs, l.subscribers)
	l.subMu.RUnlock()

	// Note: we've already released l.mu above via defer.
	// Actually we haven't — defer runs at function end. So we dispatch here
	// while still holding l.mu. Subscribers must NOT call Emit (deadlock).
	for i := range subs {
		if matchesFilters(ev, subs[i].Filters) {
			if err := subs[i].OnEvent(ev); err != nil {
				// Store error on the subscriber — caller checks later.
				// For now, we just ignore (runtime collects errors separately).
				_ = err
			}
		}
	}
}

// MergeClock merges a remote service's vector clock into the local service's clock.
// This records a causal dependency (e.g., api received a response from db).
func (l *EventLog) MergeClock(local, remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clocks[local] == nil {
		l.clocks[local] = make(map[string]int64)
	}
	if l.clocks[remote] == nil {
		return
	}
	for k, v := range l.clocks[remote] {
		if v > l.clocks[local][k] {
			l.clocks[local][k] = v
		}
	}
}

// Events returns a snapshot of all events.
func (l *EventLog) Events() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// Len returns the number of events.
func (l *EventLog) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.events)
}

// Subscribe registers a callback to be called on matching events.
// Returns a subscriber ID for later removal.
func (l *EventLog) Subscribe(filters []eventFilter, fn func(Event) error) int {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	l.nextSubID++
	l.subscribers = append(l.subscribers, Subscriber{
		ID:      l.nextSubID,
		Filters: filters,
		OnEvent: fn,
	})
	return l.nextSubID
}

// Unsubscribe removes a subscriber by ID.
func (l *EventLog) Unsubscribe(id int) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for i, s := range l.subscribers {
		if s.ID == id {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
			return
		}
	}
}

// ClearSubscribers removes all subscribers.
func (l *EventLog) ClearSubscribers() {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	l.subscribers = nil
}

// Reset clears the event log and resets the sequence counter.
func (l *EventLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = nil
	l.seq = 0
	l.clocks = make(map[string]map[string]int64)
	l.byType = make(map[string][]int)
	l.byService = make(map[string][]int)
}

// EventsByType returns a snapshot of all events with the given type,
// ordered by emission. Uses the secondary type index for efficiency.
func (l *EventLog) EventsByType(typ string) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	idxs := l.byType[typ]
	out := make([]Event, len(idxs))
	for i, idx := range idxs {
		out[i] = l.events[idx]
	}
	return out
}

// EventsByService returns a snapshot of all events from the given service.
func (l *EventLog) EventsByService(svc string) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	idxs := l.byService[svc]
	out := make([]Event, len(idxs))
	for i, idx := range idxs {
		out[i] = l.events[idx]
	}
	return out
}

// MatchingEvents returns all events matching the given matcher, in emission order.
func (l *EventLog) MatchingEvents(m *MatcherVal) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Event
	for _, ev := range l.events {
		if m.Matches(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// LastMatching returns the most recently emitted event matching m, or zero.
func (l *EventLog) LastMatching(m *MatcherVal) (Event, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if m.Matches(l.events[i]) {
			return l.events[i], true
		}
	}
	return Event{}, false
}

// FirstMatching returns the earliest emitted event matching m, or zero.
func (l *EventLog) FirstMatching(m *MatcherVal) (Event, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, ev := range l.events {
		if m.Matches(ev) {
			return ev, true
		}
	}
	return Event{}, false
}

// CountMatching returns the number of events matching m.
func (l *EventLog) CountMatching(m *MatcherVal) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, ev := range l.events {
		if m.Matches(ev) {
			n++
		}
	}
	return n
}

// FormatShiViz renders the event log in ShiViz-compatible text format.
// Output: regex line, delimiter line, then log lines.
func (l *EventLog) FormatShiViz() string {
	l.mu.RLock()
	events := make([]Event, len(l.events))
	copy(events, l.events)
	l.mu.RUnlock()

	var sb strings.Builder

	// Line 1: parsing regex — host, clock, and event on one line.
	sb.WriteString(`(?<host>\S+) (?<clock>\{.*?\}) (?<event>.*)`)
	sb.WriteByte('\n')
	// Line 2: multi-execution delimiter (empty = single execution).
	sb.WriteByte('\n')

	for _, ev := range events {
		host := ev.Service
		if host == "" {
			// Skip metadata events (fault_applied, partition, etc.) — they have no
			// service attribution and would create a spurious swimlane.
			continue
		}

		// Build event description.
		var desc string
		if ev.Type == "violation" {
			// Violation marker — stands out in ShiViz visualization.
			reason := ev.Fields["reason"]
			testName := ev.Fields["test"]
			desc = fmt.Sprintf("VIOLATION [%s] %s", testName, reason)
		} else {
			desc = ev.EventType
			if decision, ok := ev.Fields["decision"]; ok {
				desc += " " + decision
			}
			if label, ok := ev.Fields["label"]; ok && label != "" {
				desc += " [" + label + "]"
			}
			if path, ok := ev.Fields["path"]; ok && path != "" {
				desc += " " + path
			}
			if lat, ok := ev.Fields["latency_ms"]; ok && lat != "" && lat != "0" {
				desc += " (+" + lat + "ms)"
			}
			// Step events: show target and method.
			if target, ok := ev.Fields["target"]; ok {
				if method, ok := ev.Fields["method"]; ok {
					desc += " " + method + "→" + target
				}
			}
		}

		// Vector clock as JSON.
		clockJSON := formatVectorClock(ev.VectorClock)

		// ShiViz format: "host {clock} event" — all on one line.
		fmt.Fprintf(&sb, "%s %s %s\n", host, clockJSON, desc)
	}

	return sb.String()
}

// formatVectorClock renders a vector clock as a JSON object string.
func formatVectorClock(vc map[string]int64) string {
	if len(vc) == 0 {
		return "{}"
	}
	// Sort keys for deterministic output.
	keys := make([]string, 0, len(vc))
	for k := range vc {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%q: %d", k, vc[k])
	}
	sb.WriteByte('}')
	return sb.String()
}
