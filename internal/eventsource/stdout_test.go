package eventsource

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type mockDecoder struct{}

func (d *mockDecoder) Name() string { return "mock" }
func (d *mockDecoder) Decode(raw []byte) (map[string]string, error) {
	return map[string]string{"data": string(raw), "decoded": "true"}, nil
}

func TestStdoutSource(t *testing.T) {
	src := StdoutSource(&mockDecoder{})

	pr, pw := io.Pipe()

	var mu sync.Mutex
	var events []map[string]string

	cfg := SourceConfig{
		ServiceName: "test-svc",
		Emit: func(typ string, fields map[string]string) {
			mu.Lock()
			events = append(events, fields)
			mu.Unlock()
		},
	}

	ctx := context.Background()
	src.StartWithReader(ctx, cfg, pr)

	// Write some lines.
	pw.Write([]byte("line1\n"))
	pw.Write([]byte("line2\n"))
	pw.Write([]byte("\n")) // empty line — should be skipped
	pw.Write([]byte("line3\n"))
	pw.Close()

	// Wait for processing.
	time.Sleep(100 * time.Millisecond)
	src.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0]["decoded"] != "true" {
		t.Error("expected decoded=true")
	}
	if events[0]["data"] != "line1" {
		t.Errorf("expected data=line1, got %q", events[0]["data"])
	}
}

func TestStdoutSourceNoDecoder(t *testing.T) {
	src := StdoutSource(nil)

	pr, pw := io.Pipe()

	var mu sync.Mutex
	var events []map[string]string

	cfg := SourceConfig{
		Emit: func(typ string, fields map[string]string) {
			mu.Lock()
			events = append(events, fields)
			mu.Unlock()
		},
	}

	ctx := context.Background()
	src.StartWithReader(ctx, cfg, pr)

	pw.Write([]byte("raw line\n"))
	pw.Close()

	time.Sleep(100 * time.Millisecond)
	src.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0]["raw"] != "raw line" {
		t.Errorf("expected raw='raw line', got %q", events[0]["raw"])
	}
}
