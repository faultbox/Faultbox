package eventsource

import (
	"strings"
	"testing"
)

// The `topic` source is registered but not yet reachable from a spec — no
// builtin emits SourceName "topic". These tests pin the group-naming rule
// now so that wiring it up later cannot silently reintroduce the shared
// consumer group that made Kafka-consuming specs non-reproducible.

func newTopicSource(t *testing.T, params map[string]string) *topicSource {
	t.Helper()
	factory, ok := GetSource("topic")
	if !ok {
		t.Fatal("topic source not registered")
	}
	src, err := factory(params, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ts, ok := src.(*topicSource)
	if !ok {
		t.Fatalf("source type = %T, want *topicSource", src)
	}
	return ts
}

func TestTopicSource_DefaultGroupIsNotShared(t *testing.T) {
	got := newTopicSource(t, map[string]string{"topic": "order-events"}).group

	if got == "faultbox" {
		t.Fatal("default group is the shared constant; committed offsets will leak between runs")
	}
	if !strings.HasPrefix(got, "faultbox-") || !strings.HasSuffix(got, "-order-events") {
		t.Errorf("group = %q, want faultbox-<nonce>-order-events", got)
	}
}

func TestTopicSource_ExplicitGroupWins(t *testing.T) {
	// A spec about consumer-group semantics — rebalances, redelivery,
	// offset commits — needs the name it asked for.
	got := newTopicSource(t, map[string]string{"topic": "t", "group": "billing-workers"}).group
	if got != "billing-workers" {
		t.Errorf("group = %q, want billing-workers", got)
	}
}

func TestTopicSource_DefaultGroupIsStableWithinARun(t *testing.T) {
	// Two observers of one topic in the same run share a group; splitting
	// them would make each read the whole topic independently.
	a := newTopicSource(t, map[string]string{"topic": "events"}).group
	b := newTopicSource(t, map[string]string{"topic": "events"}).group
	if a != b {
		t.Errorf("groups differ within a run: %q vs %q", a, b)
	}
}

func TestTopicSource_GroupNameIsBrokerSafe(t *testing.T) {
	// Kafka rejects a group ID outside [a-zA-Z0-9._-].
	got := newTopicSource(t, map[string]string{"topic": "orders/eu west+1"}).group
	if strings.ContainsAny(got, "/ +") {
		t.Errorf("group = %q, want the unsafe characters replaced", got)
	}
}

func TestTopicSource_LongTopicIsTruncated(t *testing.T) {
	got := newTopicSource(t, map[string]string{"topic": strings.Repeat("x", 300)}).group
	if len(got) > 80+len("faultbox-")+len(groupNonce)+1 {
		t.Errorf("group length = %d, want the topic segment capped at 80", len(got))
	}
}
