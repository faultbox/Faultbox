package connowner

import (
	"net"
	"testing"
	"time"
)

type fakeConn struct{ local, remote net.Addr }

func (c fakeConn) LocalAddr() net.Addr            { return c.local }
func (c fakeConn) RemoteAddr() net.Addr           { return c.remote }
func (fakeConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

func addr(port int) net.Addr { return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port} }

func TestForwardConflictsCleanupAndHistoricalOwnership(t *testing.T) {
	tracker := newTracker(t.TempDir())
	client := fakeConn{local: addr(9000), remote: addr(40000)}
	upstream := fakeConn{local: addr(40001), remote: addr(9092)}
	cleanup := tracker.Forward(client, upstream)
	key, _ := connectionTuple(upstream.local, upstream.remote)
	tracker.mu.Lock()
	var original *Binding
	for _, b := range tracker.bridges[key] {
		original = b
	}
	tracker.mu.Unlock()
	// A proven original identity is deliberately injected only in this test:
	// production has no API for inventing attribution.
	want := Source{Service: "sut", Instance: "proven-instance", PID: 42}
	original.source = want
	observer := tracker.Bind(upstream.local, upstream.remote)
	if got := observer.Source(); got != want {
		t.Fatalf("forwarded source=%+v", got)
	}
	conflictCleanup := tracker.Forward(client, upstream)
	if got := tracker.Bind(upstream.local, upstream.remote).Source(); got.Known() {
		t.Fatalf("conflicting bridges attributed %+v", got)
	}
	cleanup()
	cleanup()
	tracker.mu.Lock()
	remaining := len(tracker.bridges[key])
	tracker.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("old cleanup removed newer bridge: %d", remaining)
	}
	if observer.Source() != want {
		t.Fatal("cached connection identity changed")
	}
	conflictCleanup()
	if tracker.Bind(upstream.local, upstream.remote).Source().Known() {
		t.Fatal("bridge leaked")
	}
}

func TestAddressNormalizationAndInvalidInput(t *testing.T) {
	v4, _ := address(&net.TCPAddr{IP: net.ParseIP("127.0.0.1").To4(), Port: 1234})
	mapped, _ := address(&net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 1234})
	if v4 != mapped {
		t.Fatalf("IPv4 mismatch %+v %+v", v4, mapped)
	}
	for _, invalid := range []net.Addr{nil, &net.UnixAddr{Name: "socket", Net: "unix"}, &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1234, Zone: "eth0"}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}} {
		if New().Bind(invalid, addr(9000)).Source().Known() {
			t.Fatal("invalid address attributed")
		}
	}
}

func TestUnsupportedPlatformStaysUnknown(t *testing.T) {
	if platformSupported {
		t.Skip("non-Linux contract")
	}
	tracker := New()
	undo, err := tracker.Register("sut", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if tracker.Bind(addr(40000), addr(9092)).Source().Known() || tracker.IsActive(Source{Service: "sut", Instance: "invented", PID: 1}) {
		t.Fatal("unsupported platform attributed a source")
	}
}

func TestForwardCycleFailsClosed(t *testing.T) {
	tracker := New()
	client := fakeConn{local: addr(9092), remote: addr(40000)}
	upstream := fakeConn{local: addr(40000), remote: addr(9092)}
	cleanup := tracker.Forward(client, upstream)
	defer cleanup()
	if source := tracker.Bind(upstream.local, upstream.remote).Source(); source.Known() {
		t.Fatalf("cyclic bridge attributed %+v", source)
	}
}
