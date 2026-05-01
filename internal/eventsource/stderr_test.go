package eventsource

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestStderrSource(t *testing.T) {
	src := StderrSource(&mockDecoder{})

	pr, pw := io.Pipe()

	var mu sync.Mutex
	var events []map[string]string
	var types []string

	cfg := SourceConfig{
		ServiceName: "test-svc",
		Emit: func(typ string, fields map[string]string) {
			mu.Lock()
			events = append(events, fields)
			types = append(types, typ)
			mu.Unlock()
		},
	}

	ctx := context.Background()
	src.StartWithReader(ctx, cfg, pr)

	pw.Write([]byte("line1\n"))
	pw.Write([]byte("line2\n"))
	pw.Write([]byte("\n"))
	pw.Write([]byte("line3\n"))
	pw.Close()

	time.Sleep(100 * time.Millisecond)
	src.Stop()

	mu.Lock()
	defer mu.Unlock()

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	// Event type discriminates stderr from stdout — the report timeline
	// renders them on the same lane but the type matters for filtering.
	for i, ty := range types {
		if ty != "stderr" {
			t.Errorf("event %d: type = %q, want stderr", i, ty)
		}
	}
	if events[0]["decoded"] != "true" {
		t.Error("expected decoded=true")
	}
	if events[0]["data"] != "line1" {
		t.Errorf("expected data=line1, got %q", events[0]["data"])
	}
}

func TestStderrSourceNoDecoder(t *testing.T) {
	src := StderrSource(nil)

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
