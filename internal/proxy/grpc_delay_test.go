package proxy

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func delayProxy(t *testing.T, delay time.Duration) (*grpcProxy, string, <-chan ProxyEvent) {
	t.Helper()
	addr, stop := echoUpstream(t)
	t.Cleanup(stop)
	events := make(chan ProxyEvent, 32)
	p := newGRPCProxy(func(e ProxyEvent) { events <- e }, "config")
	listen, err := p.Start(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop() })
	p.AddRule(Rule{Action: ActionDelay, Delay: delay})
	return p, listen, events
}

func rawCall(t *testing.T, addr string, ctx context.Context) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.ForceCodec(rawBytesCodec{})))
	if err != nil {
		return err
	}
	defer conn.Close()
	payload := []byte("delayed echo")
	var reply []byte
	err = conn.Invoke(ctx, "/example.Config/Lookup", &payload, &reply)
	if err == nil && string(reply) != string(payload) {
		t.Errorf("corrupt reply: %q", reply)
	}
	return err
}

func nextDelayEvent(t *testing.T, events <-chan ProxyEvent, phase string) ProxyEvent {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case e := <-events:
			if e.Fields["phase"] == phase {
				return e
			}
		case <-timeout.C:
			t.Fatalf("missing delay %s event", phase)
			return ProxyEvent{}
		}
	}
}

func TestGRPCDelayCancellationRecordsHitBeforeDeadline(t *testing.T) {
	_, addr, events := delayProxy(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rawCall(t, addr, ctx) }()
	hit := nextDelayEvent(t, events, "started")
	if hit.Type != "" || hit.Action != "delay" {
		t.Fatalf("not a legacy hit: %+v", hit)
	}
	cancel()
	end := nextDelayEvent(t, events, "cancelled")
	if end.Type != "proxy_delay_cancelled" || end.Fields["rpc_id"] != hit.Fields["rpc_id"] {
		t.Fatalf("bad cancellation: %+v", end)
	}
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("call: %v", err)
	}
}

func TestGRPCDelayDeadline(t *testing.T) {
	_, addr, events := delayProxy(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := rawCall(t, addr, ctx)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("call: %v", err)
	}
	nextDelayEvent(t, events, "started")
	nextDelayEvent(t, events, "cancelled")
}

func TestGRPCClearRulesKeepsListenerAndInflightDelay(t *testing.T) {
	p, addr, events := delayProxy(t, 600*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- rawCall(t, addr, ctx) }()
	hit := nextDelayEvent(t, events, "started")
	p.ClearRules()
	// A fresh connection succeeds while the matched request is still delayed.
	quick, cancelQuick := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelQuick()
	if err := rawCall(t, addr, quick); err != nil {
		t.Fatalf("new connection after clear: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("original call: %v", err)
	}
	if time.Since(start) < 600*time.Millisecond {
		t.Fatal("clearing rules shortened an in-flight delay")
	}
	end := nextDelayEvent(t, events, "completed")
	if end.Type != "proxy_delay_completed" || end.Fields["rpc_id"] != hit.Fields["rpc_id"] {
		t.Fatalf("bad completion: %+v", end)
	}
}

func TestGRPCStopCancelsInflightDelay(t *testing.T) {
	p, addr, events := delayProxy(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rawCall(t, addr, ctx) }()
	nextDelayEvent(t, events, "started")
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop waited for the entire delay")
	}
	nextDelayEvent(t, events, "cancelled")
	if err := <-done; err == nil {
		t.Fatal("stopped RPC succeeded")
	}
	select {
	case e := <-events:
		t.Fatalf("late event after Stop: %+v", e)
	default:
	}
}
