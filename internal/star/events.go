package star

import (
	"sync"
	"time"
)

// Event is a single entry in the event log.
type Event struct {
	Seq       int64             `json:"seq"`
	Timestamp time.Time         `json:"timestamp"`
	Type      string            `json:"type"`
	Service   string            `json:"service,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// EventLog is a thread-safe, append-only ordered event log.
type EventLog struct {
	mu     sync.RWMutex
	events []Event
	seq    int64
}

// NewEventLog creates a new empty event log.
func NewEventLog() *EventLog {
	return &EventLog{}
}

// Emit appends an event to the log.
func (l *EventLog) Emit(typ, service string, fields map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.events = append(l.events, Event{
		Seq:       l.seq,
		Timestamp: time.Now(),
		Type:      typ,
		Service:   service,
		Fields:    fields,
	})
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
