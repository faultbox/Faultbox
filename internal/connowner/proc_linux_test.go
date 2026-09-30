package connowner

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestProcAddressesAndExactTuples(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		order     binary.ByteOrder
	}{
		{"0100007F:90E4", "127.0.0.1:37092", binary.LittleEndian},
		{"7F000001:90E4", "127.0.0.1:37092", binary.BigEndian},
		{"00000000000000000000000001000000:9092", "[::1]:37010", binary.LittleEndian},
		{"0000000000000000FFFF00000100007F:9092", "127.0.0.1:37010", binary.LittleEndian},
		{"20010DB8000000000000000000000001:9092", "[2001:db8::1]:37010", binary.BigEndian},
		{"B80D0120000000000000000001000000:9092", "[2001:db8::1]:37010", binary.LittleEndian},
	} {
		got, ok := parseProcEndpoint(tc.raw, tc.order)
		if !ok || net.JoinHostPort(got.addr.String(), strconv.Itoa(int(got.port))) != tc.want {
			t.Fatalf("%s -> %+v, %v; want %s", tc.raw, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"garbage", "0100007F:10000", "0100007F:0000", "0100007Z:42"} {
		if _, ok := parseProcEndpoint(bad, binary.LittleEndian); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	row := "0: 0100007F:90E4 0100007F:2384 01 00000000:00000000 00:00000000 00000000 1000 0 12345"
	key, inode, ok := parseTCPRow(row, binary.LittleEndian)
	if !ok || inode != 12345 || key.from.port != 37092 || key.to.port != 9092 {
		t.Fatalf("bad row: %+v %d %v", key, inode, ok)
	}
	if _, _, ok := parseTCPRow(strings.Replace(row, " 01 ", " 0A ", 1), binary.LittleEndian); ok {
		t.Fatal("listener must not prove a client owner")
	}
}

func fakeProcess(t *testing.T, root string, pid, parent int, start uint64, children string) {
	t.Helper()
	base := filepath.Join(root, strconv.Itoa(pid))
	for _, dir := range []string{"fd", "net", "task/" + strconv.Itoa(pid)} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[19] = "S", strconv.Itoa(parent), strconv.FormatUint(start, 10)
	writeFile(t, filepath.Join(base, "stat"), fmt.Sprintf("%d (a tricky ) process) %s\n", pid, strings.Join(fields, " ")))
	writeFile(t, filepath.Join(base, "task", strconv.Itoa(pid), "children"), children)
	writeFile(t, filepath.Join(base, "net", "tcp"), "")
	writeFile(t, filepath.Join(base, "net", "tcp6"), "")
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeSocket(t *testing.T, root string, pid int, inode uint64) {
	t.Helper()
	base := filepath.Join(root, strconv.Itoa(pid))
	if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(base, "fd", "7")); err != nil {
		t.Fatal(err)
	}
	// procfs follows the test machine's native endianness.
	var raw [4]byte
	binary.NativeEndian.PutUint32(raw[:], 0x7f000001)
	addr := fmt.Sprintf("%X", raw)
	writeFile(t, filepath.Join(base, "net", "tcp"), fmt.Sprintf("0: %s:90E4 %s:2384 01 0:0 00:0 0 1000 0 %d\n", addr, addr, inode))
}

func addresses() (net.Addr, net.Addr) {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 37092}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9092}
}

func TestRegisteredDescendantPIDReuseAndLifetime(t *testing.T) {
	root := t.TempDir()
	fakeProcess(t, root, 100, 1, 10, "101")
	fakeProcess(t, root, 101, 100, 11, "")
	fakeSocket(t, root, 101, 123)
	tracker := newTracker(root)
	peer, local := addresses()
	binding := tracker.Bind(peer, local)
	if binding.Source().Known() {
		t.Fatal("unregistered tree attributed")
	}
	unregister, err := tracker.Register("sut", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	source := binding.Source()
	if !source.Known() || source.Service != "sut" || source.PID != 100 || !tracker.IsActive(source) {
		t.Fatalf("source=%+v", source)
	}
	binding.Close()
	if binding.Source() != source {
		t.Fatal("lost historical source on close")
	}
	unregister()
	if tracker.IsActive(source) {
		t.Fatal("unregistered generation still active")
	}
	fakeProcess(t, root, 100, 1, 20, "")
	fakeSocket(t, root, 100, 124)
	undo, err := tracker.Register("sut", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	newSource := tracker.Bind(peer, local).Source()
	if !newSource.Known() || newSource.Instance == source.Instance || tracker.IsActive(source) {
		t.Fatalf("reused PID source=%+v old=%+v", newSource, source)
	}
	unregister() // A stale cleanup must not revoke the new generation.
	if !tracker.IsActive(newSource) {
		t.Fatal("stale unregister affected new generation")
	}
	fakeProcess(t, root, 100, 1, 30, "")
	if tracker.IsActive(newSource) {
		t.Fatal("kernel PID reuse was not detected")
	}
}

func TestUnknownClosedBindingCannotClaimReusedTuple(t *testing.T) {
	root := t.TempDir()
	tracker := newTracker(root)
	peer, local := addresses()
	binding := tracker.Bind(peer, local)
	if binding.Source().Known() {
		t.Fatal("unexpected source")
	}
	binding.Close()
	fakeProcess(t, root, 100, 1, 10, "")
	fakeSocket(t, root, 100, 123)
	undo, err := tracker.Register("new", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if binding.Source().Known() {
		t.Fatal("closed unproven binding acquired new tuple's owner")
	}
	if !tracker.Bind(peer, local).Source().Known() {
		t.Fatal("new connection did not resolve")
	}
}

func TestSharedSocketAndUnverifiedTreeAreUnknown(t *testing.T) {
	for _, tc := range []string{"shared", "wrong-parent", "missing-fd-directory", "unreadable-stat"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			tracker := newTracker(root)
			peer, local := addresses()
			fakeProcess(t, root, 100, 1, 10, "")
			fakeProcess(t, root, 200, 1, 20, "")
			fakeSocket(t, root, 200, 123)
			a, err := tracker.Register("a", 100)
			if err != nil {
				t.Fatal(err)
			}
			defer a()
			switch tc {
			case "shared":
				fakeSocket(t, root, 100, 123)
				b, err := tracker.Register("b", 200)
				if err != nil {
					t.Fatal(err)
				}
				defer b()
			case "wrong-parent":
				writeFile(t, filepath.Join(root, "100/task/100/children"), "200")
			case "missing-fd-directory":
				b, err := tracker.Register("b", 200)
				if err != nil {
					t.Fatal(err)
				}
				defer b()
				if err := os.RemoveAll(filepath.Join(root, "100/fd")); err != nil {
					t.Fatal(err)
				}
			case "unreadable-stat":
				b, err := tracker.Register("b", 200)
				if err != nil {
					t.Fatal(err)
				}
				defer b()
				// A malformed/unreadable registered process is uncertain, not
				// proof that the other tree owns this connection exclusively.
				writeFile(t, filepath.Join(root, "100/stat"), "incomplete")
			}
			if source := tracker.Bind(peer, local).Source(); source.Known() {
				t.Fatalf("unsafe attribution %+v", source)
			}
		})
	}
}

func TestConcurrentBindingAndUnregister(t *testing.T) {
	root := t.TempDir()
	fakeProcess(t, root, 100, 1, 10, "")
	fakeSocket(t, root, 100, 123)
	tracker := newTracker(root)
	undo, err := tracker.Register("sut", 100)
	if err != nil {
		t.Fatal(err)
	}
	peer, local := addresses()
	binding := tracker.Bind(peer, local)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				source := binding.Source()
				tracker.IsActive(source)
			}
			binding.Close()
			undo()
		}()
	}
	wg.Wait()
}

func TestOwnershipRequiresDestinationAndDoesNotCrossTrackers(t *testing.T) {
	root := t.TempDir()
	fakeProcess(t, root, 100, 1, 10, "")
	fakeSocket(t, root, 100, 123)
	a, b := newTracker(root), newTracker(root)
	undoA, err := a.Register("sut", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer undoA()
	undoB, err := b.Register("sut", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer undoB()
	peer, local := addresses()
	wrongDestination := &net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 9092}
	if source := a.Bind(peer, wrongDestination).Source(); source.Known() {
		t.Fatalf("matched only source tuple: %+v", source)
	}
	sourceA := a.Bind(peer, local).Source()
	sourceB := b.Bind(peer, local).Source()
	if !sourceA.Known() || !sourceB.Known() || sourceA.Instance == sourceB.Instance || a.IsActive(sourceB) || b.IsActive(sourceA) {
		t.Fatalf("tracker instance leak: %+v %+v", sourceA, sourceB)
	}
}
