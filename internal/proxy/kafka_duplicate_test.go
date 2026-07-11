package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// countingKafkaUpstream echoes length-prefixed frames and counts how many it
// receives. Returns (addr, *counter, stop).
func countingKafkaUpstream(t *testing.T) (string, *int32, func()) {
	t.Helper()
	var frames int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					hdr := make([]byte, 4)
					if _, err := io.ReadFull(c, hdr); err != nil {
						return
					}
					n := int(binary.BigEndian.Uint32(hdr))
					if n <= 0 || n > 64*1024 {
						return
					}
					body := make([]byte, n)
					if _, err := io.ReadFull(c, body); err != nil {
						return
					}
					atomic.AddInt32(&frames, 1)
					c.Write(hdr)
					c.Write(body)
				}
			}()
		}
	}()
	return ln.Addr().String(), &frames, func() { ln.Close() }
}

// TestKafkaProxy_DuplicateForwardsTwice guards #138: a duplicate(topic=) rule
// must make a produce land on the broker twice (the consumer sees it twice),
// while the producer still gets a single ack. Previously ActionDuplicate had
// no case in the Kafka proxy, so the rule was a silent no-op.
func TestKafkaProxy_DuplicateForwardsTwice(t *testing.T) {
	upstreamAddr, frames, stop := countingKafkaUpstream(t)
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
	upstreamAddr, frames, stop := countingKafkaUpstream(t)
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
