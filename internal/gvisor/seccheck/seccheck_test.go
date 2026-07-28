package seccheck

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/sentry/seccheck/points/points_go_proto"
)

// frame wraps a marshalled point in the wire header the Sentry sends.
func frame(t *testing.T, msgType pb.MessageType, m proto.Message, dropped uint32) []byte {
	t.Helper()
	payload, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	buf := make([]byte, headerStructSize+len(payload))
	binary.NativeEndian.PutUint16(buf[0:2], uint16(headerStructSize))
	binary.NativeEndian.PutUint16(buf[2:4], uint16(msgType))
	binary.NativeEndian.PutUint32(buf[4:8], dropped)
	copy(buf[headerStructSize:], payload)
	return buf
}

// collector is a Sink plus the operations it decoded.
type collector struct {
	sink *Sink
	mu   sync.Mutex
	ios  []FileIO
	errs []error
}

// requireSeqpacket skips tests that need a live SOCK_SEQPACKET listener.
//
// SOCK_SEQPACKET Unix sockets are Linux-only; macOS returns "protocol not
// supported". Only the server half needs skipping — decodePoint and the
// fixture replay below run everywhere, which is the property that makes the
// decoder testable with no runsc and no Linux.
func requireSeqpacket(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("SOCK_SEQPACKET is Linux-only (this is %s); decode tests still run here", runtime.GOOS)
	}
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	requireSeqpacket(t)
	c := &collector{}
	// A short path: the default temp dir on macOS is long enough to exceed the
	// 104-byte sun_path limit, which fails with a confusing "invalid argument".
	sock := filepath.Join(t.TempDir(), "s.sock")
	if len(sock) > 100 {
		sock = filepath.Join(os.TempDir(), "fbsc.sock")
	}
	s, err := Listen(Config{
		Path:     sock,
		OnFileIO: func(f FileIO) { c.mu.Lock(); c.ios = append(c.ios, f); c.mu.Unlock() },
		OnError:  func(e error) { c.mu.Lock(); c.errs = append(c.errs, e); c.mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	c.sink = s
	t.Cleanup(func() { s.Close() })
	return c
}

func (c *collector) events() []FileIO {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]FileIO, len(c.ios))
	copy(out, c.ios)
	return out
}

func (c *collector) errors() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]error, len(c.errs))
	copy(out, c.errs)
	return out
}

// connectAndHandshake plays the Sentry side: write our Handshake, read theirs.
func connectAndHandshake(t *testing.T, sock string) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	out, _ := proto.Marshal(&pb.Handshake{Version: protocolVersion})
	if _, err := c.Write(out); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	buf := make([]byte, 4096)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read handshake reply: %v", err)
	}
	var hs pb.Handshake
	if err := proto.Unmarshal(buf[:n], &hs); err != nil {
		t.Fatalf("unmarshal handshake reply: %v", err)
	}
	if hs.GetVersion() != protocolVersion {
		t.Fatalf("handshake reply version = %d, want %d", hs.GetVersion(), protocolVersion)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func TestHandshakeAndDecodeWrite(t *testing.T) {
	c := newCollector(t)
	conn := connectAndHandshake(t, c.sink.Path())

	conn.Write(frame(t, pb.MessageType_MESSAGE_SYSCALL_WRITE, &pb.Write{
		ContextData: &pb.ContextData{
			ThreadGroupId: 42, ThreadId: 43,
			ProcessName: "postgres", ContainerId: "abc", TimeNs: 1234,
		},
		Exit:      &pb.Exit{Result: 8192},
		Sysno:     68, // pwrite64 on aarch64
		Fd:        7,
		FdPath:    "/var/lib/postgresql/data/pg_wal/000000010000000000000001",
		Count:     8192,
		HasOffset: true,
		Offset:    7815168,
	}, 0))

	if !waitFor(t, func() bool { return len(c.events()) == 1 }) {
		t.Fatalf("no event decoded (errors: %v)", c.errors())
	}
	got := c.events()[0]
	if got.Op != OpWrite {
		t.Errorf("Op = %q, want write", got.Op)
	}
	if got.Path != "/var/lib/postgresql/data/pg_wal/000000010000000000000001" {
		t.Errorf("Path = %q", got.Path)
	}
	if got.Count != 8192 || got.Result != 8192 {
		t.Errorf("count/result = %d/%d, want 8192/8192", got.Count, got.Result)
	}
	if !got.HasOffset || got.Offset != 7815168 {
		t.Errorf("offset = %d (has=%v), want 7815168", got.Offset, got.HasOffset)
	}
	if !got.Positional {
		t.Error("pwrite64 not flagged positional")
	}
	if got.PID != 42 || got.TID != 43 || got.ProcessName != "postgres" {
		t.Errorf("context not decoded: %+v", got)
	}
}

// TestWriteVsPwriteBySysno is the M0.3 finding: pwrite64 and write share
// MESSAGE_SYSCALL_WRITE, so a decoder switching on message type alone
// conflates them and reports a bogus offset for sequential writes.
func TestWriteVsPwriteBySysno(t *testing.T) {
	tests := []struct {
		name           string
		sysno          uint64
		wantPositional bool
	}{
		{"write aarch64", 64, false},
		{"pwrite64 aarch64", 68, true},
		{"write amd64", 1, false},
		{"pwrite64 amd64", 18, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			io, ok, err := decodePoint(uint16(pb.MessageType_MESSAGE_SYSCALL_WRITE), mustMarshal(t, &pb.Write{
				Sysno: tc.sysno, FdPath: "/data/x", Count: 10,
				Exit: &pb.Exit{Result: 10},
			}))
			if err != nil || !ok {
				t.Fatalf("decode: ok=%v err=%v", ok, err)
			}
			if io.Positional != tc.wantPositional {
				t.Errorf("sysno %d: Positional = %v, want %v", tc.sysno, io.Positional, tc.wantPositional)
			}
		})
	}
}

// TestOpenatPathAssembly is the other M0.3 finding: Open.pathname is the raw
// syscall argument and frequently relative, so an absolute path has to be
// assembled from the dirfd's fd_path or the cwd.
func TestOpenatPathAssembly(t *testing.T) {
	tests := []struct {
		name     string
		open     *pb.Open
		wantPath string
	}{
		{
			"absolute pathname used as-is",
			&pb.Open{Pathname: "/etc/passwd", FdPath: "/ignored"},
			"/etc/passwd",
		},
		{
			"relative pathname joined to dirfd path",
			&pb.Open{Pathname: "global/pg_filenode.map", FdPath: "/var/lib/postgresql/data"},
			"/var/lib/postgresql/data/global/pg_filenode.map",
		},
		{
			"relative pathname falls back to cwd",
			&pb.Open{
				Pathname:    "base/5/PG_VERSION",
				ContextData: &pb.ContextData{Cwd: "/var/lib/postgresql/data"},
			},
			"/var/lib/postgresql/data/base/5/PG_VERSION",
		},
		{
			"unresolvable relative path stays relative rather than inventing a root",
			&pb.Open{Pathname: "postmaster.pid"},
			"postmaster.pid",
		},
		{
			"no pathname falls back to fd_path",
			&pb.Open{FdPath: "/proc/self/fd"},
			"/proc/self/fd",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			io, ok, err := decodePoint(uint16(pb.MessageType_MESSAGE_SYSCALL_OPEN), mustMarshal(t, tc.open))
			if err != nil || !ok {
				t.Fatalf("decode: ok=%v err=%v", ok, err)
			}
			if io.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", io.Path, tc.wantPath)
			}
		})
	}
}

func TestDecodeFailedOpenCarriesRealErrno(t *testing.T) {
	io, ok, err := decodePoint(uint16(pb.MessageType_MESSAGE_SYSCALL_OPEN), mustMarshal(t, &pb.Open{
		Pathname: "/lib/libpq.so.5",
		Exit:     &pb.Exit{Result: 0, Errorno: 2}, // ENOENT
	}))
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if io.Errno != 2 {
		t.Errorf("Errno = %d, want 2 (ENOENT) — a real errno, not an inference", io.Errno)
	}
}

func TestShortTransferDetection(t *testing.T) {
	cases := []struct {
		name string
		io   FileIO
		want bool
	}{
		{"full write", FileIO{Op: OpWrite, Count: 100, Result: 100}, false},
		{"short write", FileIO{Op: OpWrite, Count: 100, Result: 40}, true},
		{"short read", FileIO{Op: OpRead, Count: 1024, Result: 3}, true},
		{"failed write is not short", FileIO{Op: OpWrite, Count: 100, Result: -1, Errno: 28}, false},
		{"open is never short", FileIO{Op: OpOpen, Count: 0, Result: 5}, false},
	}
	for _, tc := range cases {
		if got := tc.io.ShortTransfer(); got != tc.want {
			t.Errorf("%s: ShortTransfer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnknownPointIgnored covers the protocol's forward-compatibility rule:
// a monitor must ignore point types it does not understand rather than error.
func TestUnknownPointIgnored(t *testing.T) {
	io, ok, err := decodePoint(uint16(pb.MessageType_MESSAGE_SENTRY_CLONE), []byte{})
	if err != nil {
		t.Errorf("unknown point type produced an error: %v", err)
	}
	if ok {
		t.Errorf("unknown point type was decoded as %+v", io)
	}
}

// TestRejectsOversizedAndMalformedFields: the Sentry is untrusted in gVisor's
// own threat model, so hostile input must be bounded rather than trusted.
func TestRejectsOversizedAndMalformedFields(t *testing.T) {
	huge := make([]byte, maxPathLen*4)
	for i := range huge {
		huge[i] = 'A'
	}
	io, ok, err := decodePoint(uint16(pb.MessageType_MESSAGE_SYSCALL_WRITE), mustMarshal(t, &pb.Write{
		FdPath: string(huge), Count: 1, Exit: &pb.Exit{Result: 1},
	}))
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if len(io.Path) != maxPathLen {
		t.Errorf("path length = %d, want it capped at %d", len(io.Path), maxPathLen)
	}

	if _, _, err := decodePoint(uint16(pb.MessageType_MESSAGE_SYSCALL_WRITE), []byte{0xFF, 0xFF, 0xFF}); err == nil {
		t.Error("malformed protobuf decoded without error")
	}
}

func TestMalformedHeaderRejected(t *testing.T) {
	c := newCollector(t)
	conn := connectAndHandshake(t, c.sink.Path())

	// Too short to contain a header.
	conn.Write([]byte{1, 2})
	// Header claims a size past the end of the message.
	bad := make([]byte, headerStructSize+4)
	binary.NativeEndian.PutUint16(bad[0:2], 9999)
	conn.Write(bad)

	if !waitFor(t, func() bool { return len(c.errors()) >= 2 }) {
		t.Errorf("malformed frames produced %d errors, want >= 2", len(c.errors()))
	}
	if got := c.sink.Points(); got != 0 {
		t.Errorf("decoded %d points from malformed input, want 0", got)
	}
}

func TestDroppedCountSurfaced(t *testing.T) {
	c := newCollector(t)
	conn := connectAndHandshake(t, c.sink.Path())

	conn.Write(frame(t, pb.MessageType_MESSAGE_SYSCALL_WRITE, &pb.Write{
		FdPath: "/data/x", Count: 1, Exit: &pb.Exit{Result: 1},
	}, 17))

	if !waitFor(t, func() bool { return c.sink.Dropped() == 17 }) {
		t.Errorf("Dropped = %d, want 17 — incomplete observation must be visible", c.sink.Dropped())
	}
}

func TestCloseIsIdempotentAndRemovesSocket(t *testing.T) {
	c := newCollector(t)
	sock := c.sink.Path()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if err := c.sink.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if _, err := os.Stat(sock); err == nil {
		t.Error("socket file survived Close")
	}
	if err := c.sink.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	c := newCollector(t)
	fi, err := os.Stat(c.sink.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The socket carries the SUT's filesystem activity.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

// ─── fixture replay ────────────────────────────────────────────────────────

type fixtureMsg struct {
	MessageType uint16 `json:"message_type"`
	Payload     []byte `json:"payload"`
}

// TestDecodeCapturedFixture replays a real point stream captured from runsc
// against a live Postgres. It is what makes the decoder testable on macOS with
// no runsc and no Linux — the property the M5 plan depends on.
func TestDecodeCapturedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "postgres-write.json"))
	if err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	var msgs []fixtureMsg
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("fixture is empty")
	}

	var writes, opens, withOffset, failedOpens int
	bytesPerPath := map[string]int64{}
	for _, m := range msgs {
		io, ok, err := decodePoint(m.MessageType, m.Payload)
		if err != nil {
			t.Fatalf("decode failed on a real captured point: %v", err)
		}
		if !ok {
			continue
		}
		switch io.Op {
		case OpWrite:
			writes++
			if io.HasOffset {
				withOffset++
			}
			if io.Result > 0 && io.Path != "" {
				bytesPerPath[io.Path] += io.Result
			}
		case OpOpen:
			opens++
			if io.Errno != 0 {
				failedOpens++
			}
		}
	}

	if writes == 0 {
		t.Error("no writes decoded from the fixture")
	}
	if withOffset == 0 {
		t.Error("no write carried a byte offset; positional I/O detection is broken")
	}
	if failedOpens == 0 {
		t.Error("no failed open decoded; real-errno reporting is unverified")
	}
	// The per-path byte accounting is what file_io().bytes_written is built on.
	var sawWAL bool
	for p := range bytesPerPath {
		if filepath.Base(filepath.Dir(p)) == "pg_wal" {
			sawWAL = true
		}
	}
	if !sawWAL {
		t.Errorf("no pg_wal path in per-path accounting; got %d paths", len(bytesPerPath))
	}
	t.Logf("fixture: %d writes (%d positional), %d opens (%d failed), %d distinct paths",
		writes, withOffset, opens, failedOpens, len(bytesPerPath))
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
