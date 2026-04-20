//go:build linux

package container

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestWaitForListenerFd_HappyPathLogsEveryPhase verifies RFC-022 Phase 0:
// the instrumented handoff emits a start+done pair for every phase on the
// normal success path. A regression that accidentally skips a phase (say,
// a refactor that short-circuits read_scm) shows up as a missing pair in
// the captured log stream, catching the drift before a customer hangs on
// a 3-minute timeout to report it.
func TestWaitForListenerFd_HappyPathLogsEveryPhase(t *testing.T) {
	// Capture JSON log output in memory.
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "fd.sock")

	// Fake shim side: connect, send an SCM_RIGHTS fd, read the ACK.
	// Use /dev/null as the fd to pass — it's always available, readable,
	// and won't confuse any downstream consumer.
	devNull, err := os.OpenFile("/dev/null", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer devNull.Close()

	// Start the host-side handoff goroutine first — it creates the socket.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		gotFd    int
		gotErr   error
		hostDone sync.WaitGroup
	)
	hostDone.Add(1)
	go func() {
		defer hostDone.Done()
		gotFd, gotErr = waitForListenerFd(ctx, socketPath, logger)
	}()

	// Wait for the socket file to appear (host-side Listen is fast but not
	// synchronous with this goroutine).
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Fake shim: dial + send the fd.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	defer conn.Close()
	uc := conn.(*net.UnixConn)

	rights := unix.UnixRights(int(devNull.Fd()))
	if _, _, err := uc.WriteMsgUnix([]byte("fd"), rights, nil); err != nil {
		t.Fatalf("WriteMsgUnix: %v", err)
	}

	// Read the host's ACK byte.
	ack := make([]byte, 1)
	if _, err := uc.Read(ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack[0] != 'k' {
		t.Fatalf("ack byte = %q, want 'k'", ack[0])
	}

	hostDone.Wait()
	if gotErr != nil {
		t.Fatalf("waitForListenerFd error: %v", gotErr)
	}
	if gotFd < 0 {
		t.Fatalf("waitForListenerFd returned fd = %d", gotFd)
	}
	// Close the received fd — it's a dup of /dev/null owned by the handoff.
	unix.Close(gotFd)

	// Parse the captured JSON lines and check every expected phase pair fired.
	expectedPhases := []string{"listen", "chmod", "accept", "read_scm", "parse_scm", "extract_fd", "send_ack"}
	seenStart := make(map[string]bool)
	seenDone := make(map[string]bool)

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		phase, _ := evt["phase"].(string)
		step, _ := evt["step"].(string)
		if phase == "" || step == "" {
			continue
		}
		if step == "start" {
			seenStart[phase] = true
		}
		if step == "done" {
			seenDone[phase] = true
		}
	}

	for _, p := range expectedPhases {
		if !seenStart[p] {
			t.Errorf("phase %q: missing step=start event", p)
		}
		if !seenDone[p] {
			t.Errorf("phase %q: missing step=done event", p)
		}
	}
}

// TestWaitForListenerFd_HangExitsOnContextCancel verifies the "3-minute
// hang that motivated this RFC" is surfaced cleanly: when the shim never
// connects, the host side logs the canceled accept (not a silent nothing)
// and returns ctx.Err().
func TestWaitForListenerFd_HangExitsOnContextCancel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "fd.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := waitForListenerFd(ctx, socketPath, logger)
	if err == nil {
		t.Fatal("expected error on context cancel, got nil")
	}

	// Log must contain a canceled accept event so an operator can tell at a
	// glance that the shim never arrived.
	if !strings.Contains(buf.String(), `"phase":"accept"`) ||
		!strings.Contains(buf.String(), `"step":"canceled"`) {
		t.Errorf("expected accept-canceled event in logs, got:\n%s", buf.String())
	}
}
