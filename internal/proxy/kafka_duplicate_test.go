package proxy

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestKafkaProxy_DuplicateForwardsTwice guards #138: a duplicate(topic=) rule
// must make a produce land on the broker twice (the consumer sees it twice),
// while the producer still gets a single ack. Previously ActionDuplicate had
// no case in the Kafka proxy, so the rule was a silent no-op.
func TestKafkaProxy_DuplicateForwardsTwice(t *testing.T) {
	upstreamAddr, _, frames, stop := kafkaEcho(t, false)
	defer stop()

	var dupEvents int32
	p := newKafkaProxy(func(evt ProxyEvent) {
		if evt.Action == "duplicate" {
			atomic.AddInt32(&dupEvents, 1)
		}
	}, "broker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	p.AddRule(Rule{Topic: "orders", Action: ActionDuplicate})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIProduce, "orders")
	readKafkaFrame(t, c) // producer receives exactly one ack

	// The duplicate is re-sent after the client ack; poll for the 2nd frame.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(frames) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(frames); got != 2 {
		t.Errorf("upstream received %d produce frames, want 2 (duplicate was a no-op, #138)", got)
	}
	if got := atomic.LoadInt32(&dupEvents); got != 1 {
		t.Errorf("duplicate events emitted = %d, want 1", got)
	}
}

// TestKafkaProxy_DuplicateIgnoresFetch ensures only produce is duplicated —
// duplicating a fetch is meaningless.
func TestKafkaProxy_DuplicateIgnoresFetch(t *testing.T) {
	upstreamAddr, _, frames, stop := kafkaEcho(t, false)
	defer stop()

	p := newKafkaProxy(nil, "broker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	p.AddRule(Rule{Topic: "orders", Action: ActionDuplicate})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIFetch, "orders")
	readKafkaFrame(t, c)

	// Give any (erroneous) duplicate a chance to arrive, then assert exactly one.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(frames); got != 1 {
		t.Errorf("fetch produced %d upstream frames, want 1 (fetch must not duplicate)", got)
	}
}

// TestKafkaProxy_DuplicateRuleDoesNotShadowLaterRules guards the rule-
// evaluation semantics: a duplicate rule matching a FETCH does not apply,
// and must fall through to later rules. Pre-fix the ActionDuplicate case
// returned early for fetches, silently disabling every rule after it.
func TestKafkaProxy_DuplicateRuleDoesNotShadowLaterRules(t *testing.T) {
	upstreamAddr, _, _, stop := kafkaEcho(t, false)
	defer stop()

	p := newKafkaProxy(nil, "broker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	p.AddRule(Rule{Topic: "orders", Action: ActionDuplicate})
	p.AddRule(Rule{Topic: "orders", Action: ActionError, Error: "broker down"})

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIFetch, "orders")

	// The error rule must fire for the fetch: the proxy closes the client
	// connection instead of forwarding. A successful echo response here
	// means the duplicate rule shadowed the error rule.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err == nil {
		t.Error("fetch got a response; the error rule after duplicate never fired (rule shadowing)")
	}
}
