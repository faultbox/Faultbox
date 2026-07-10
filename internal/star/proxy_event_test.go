package star

import (
	"testing"

	"github.com/faultbox/Faultbox/internal/proxy"
)

// TestEmitProxyEvent_IncludesAction guards #141: the ProxyEvent.Action (and
// Protocol) struct fields must reach the event log's Fields map so predicates
// and monitors can key on e.data.get("action"). Previously only evt.Fields
// was emitted, so `action` was always None and the documented
// assert_eventually(...action == "error") pattern could never fire.
func TestEmitProxyEvent_IncludesAction(t *testing.T) {
	rt := New(testLogger())
	rt.emitProxyEvent(proxy.ProxyEvent{
		Action:   "error",
		Protocol: "postgres",
		To:       "db",
		Fields:   map[string]string{"query": "INSERT INTO orders"},
	})

	events := rt.events.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Fields["action"] != "error" {
		t.Errorf("action field = %q, want %q (#141: action was dropped)", ev.Fields["action"], "error")
	}
	if ev.Fields["protocol"] != "postgres" {
		t.Errorf("protocol field = %q, want %q", ev.Fields["protocol"], "postgres")
	}
	if ev.Fields["query"] != "INSERT INTO orders" {
		t.Errorf("pre-existing field lost: query = %q", ev.Fields["query"])
	}
}

// TestEmitProxyEvent_DoesNotClobberExplicitFields ensures a fault plugin that
// already put "action"/"protocol" in Fields wins over the struct fields.
func TestEmitProxyEvent_DoesNotClobberExplicitFields(t *testing.T) {
	rt := New(testLogger())
	rt.emitProxyEvent(proxy.ProxyEvent{
		Action: "error",
		To:     "db",
		Fields: map[string]string{"action": "explicit"},
	})
	if got := rt.events.Events()[0].Fields["action"]; got != "explicit" {
		t.Errorf("explicit action field clobbered: got %q, want %q", got, "explicit")
	}
}
