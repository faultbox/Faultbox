package protocol

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUDPProtocolRegistered(t *testing.T) {
	p, ok := Get("udp")
	if !ok {
		t.Fatal("udp protocol not registered")
	}
	want := []string{"send", "send_no_reply"}
	if !reflect.DeepEqual(p.Methods(), want) {
		t.Errorf("Methods() = %v, want %v", p.Methods(), want)
	}
}

// TestUDP_SendAndReceive verifies the step roundtrips a datagram through a
// local echo server.
func TestUDP_SendAndReceive(t *testing.T) {
	addr, stop := startUDPEcho(t)
	defer stop()

	p := &udpProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "send", map[string]any{"data": "hello"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !res.Success {
		t.Fatalf("step failed: %s", res.Error)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	raw, _ := hex.DecodeString(body["raw"].(string))
	if string(raw) != "hello" {
		t.Errorf("echoed data = %q, want %q", string(raw), "hello")
	}
}

// TestUDP_HexPayload exercises the hex= kwarg — used for binary protocols
// like DNS that can't be expressed as utf-8 strings.
func TestUDP_HexPayload(t *testing.T) {
	addr, stop := startUDPEcho(t)
	defer stop()

	p := &udpProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "send", map[string]any{"hex": "deadbeef"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["raw"] != "deadbeef" {
		t.Errorf("raw = %v, want deadbeef", body["raw"])
	}
}

func TestUDP_SendNoReply(t *testing.T) {
	addr, stop := startUDPDiscard(t)
	defer stop()

	p := &udpProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "send_no_reply", map[string]any{"data": "fire-and-forget"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !res.Success {
		t.Fatalf("step failed: %s", res.Error)
	}
}

func TestUDP_MissingPayload(t *testing.T) {
	p := &udpProtocol{}
	res, err := p.ExecuteStep(context.Background(), "127.0.0.1:1", "send", map[string]any{})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if res.Success {
		t.Error("expected failure for missing payload")
	}
	if !strings.Contains(res.Error, "data=") {
		t.Errorf("error = %q, want mention of data=", res.Error)
	}
}

// startUDPEcho starts a UDP echo server on a random port.
func startUDPEcho(t *testing.T) (string, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64*1024)
		for {
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			conn.WriteToUDP(buf[:n], src)
		}
	}()
	return conn.LocalAddr().String(), func() {
		close(done)
		conn.Close()
	}
}

// startUDPDiscard starts a UDP server that reads but never replies.
func startUDPDiscard(t *testing.T) (string, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64*1024)
		for {
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			conn.ReadFromUDP(buf)
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return conn.LocalAddr().String(), func() {
		close(done)
		conn.Close()
	}
}
