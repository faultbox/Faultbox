package star

import (
	"context"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/gvisor/seccheck"
)

const watchSpecPrefix = `
determinism(runtime = "gvisor")
db = service("db", "/bin/true", interface("main", "tcp", 5432))
`

func loadWatchSpec(t *testing.T, body string) error {
	t.Helper()
	rt := New(testLogger())
	return rt.LoadString("test.star", watchSpecPrefix+body)
}

func runWatchTest(t *testing.T, body string) TestResult {
	t.Helper()
	rt := New(testLogger())
	src := watchSpecPrefix + body
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("spec load: %v", err)
	}
	return rt.RunTest(context.Background(), "test_w")
}

// TestFsyncOpRejectedAtSpecLoad is the M0.3 finding made user-visible.
//
// gVisor traces 42 syscalls and fsync is not among them, so accepting
// ops=["fsync"] would emit nothing — and a durability audit would "pass"
// having observed no fsync because none could ever be reported. That is
// strictly worse than refusing the spec.
func TestFsyncOpRejectedAtSpecLoad(t *testing.T) {
	for _, op := range []string{"fsync", "fdatasync", "msync", "sync_file_range", "sync"} {
		t.Run(op, func(t *testing.T) {
			res := runWatchTest(t, `
def test_w():
    watch(db, files = ["/data/**"], ops = ["`+op+`"], run = lambda: None)
`)
			if res.Result != "fail" {
				t.Fatalf("ops=[%q] was accepted; it can never emit an event", op)
			}
			if !strings.Contains(res.Reason, "no "+op+" trace point") {
				t.Errorf("error should explain gVisor has no %s trace point, got: %s", op, res.Reason)
			}
			if !strings.Contains(res.Reason, "durability") {
				t.Errorf("error should warn that write ordering is not durability, got: %s", res.Reason)
			}
		})
	}
}

func TestWatchRejectsUnknownOp(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch(db, files = ["/data/**"], ops = ["mmap"], run = lambda: None)
`)
	if res.Result != "fail" {
		t.Fatal("ops=[\"mmap\"] was accepted")
	}
	if !strings.Contains(res.Reason, "unknown operation") {
		t.Errorf("reason = %s", res.Reason)
	}
}

func TestWatchRejectsUnknownKwarg(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch(db, paths = ["/data/**"], run = lambda: None)
`)
	if res.Result != "fail" {
		t.Fatal("watch(paths=) was accepted; the kwarg is files=")
	}
	if !strings.Contains(res.Reason, "unknown keyword argument") {
		t.Errorf("reason = %s", res.Reason)
	}
}

// TestWatchIsRejectedInThisRelease pins the v0.14.0 decision.
//
// The sink and DSL are complete, but `runsc trace create` instruments only
// tasks created after the session starts, and Faultbox attaches one after the
// service is healthy. Measured: 2 trace points for a network-driven workload
// against a running Postgres backend, versus 1054 for the same work from a
// newly-spawned process. A watch would therefore observe almost nothing and
// its assertions would be vacuous, so it is refused rather than shipped with a
// caveat (RFC-054 decision record M5).
func TestWatchIsRejectedInThisRelease(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch(db, files = ["/data/**"], run = lambda: None)
`)
	if res.Result != "fail" {
		t.Fatal("watch() was accepted; it cannot observe reliably in this release")
	}
	// The message must name a release that has NOT shipped. It said "v0.14.1"
	// until v0.14.1 shipped without watch(), at which point the guidance users
	// saw was simply false — the hazard of naming a version in a user-facing
	// string. Assert on both the RFC and the target so the pair stays honest.
	for _, want := range []string{"RFC-056", "v0.15.0"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason should name %s, got: %s", want, res.Reason)
		}
	}
	if !strings.Contains(res.Reason, "Packet faults") {
		t.Errorf("reason should say packet faults still work, got: %s", res.Reason)
	}
}

func TestWatchRequiresRunCallback(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch(db, files = ["/data/**"])
`)
	if res.Result != "fail" || !strings.Contains(res.Reason, "watch_start") {
		t.Errorf("watch() without run= should point at watch_start(), got: %s", res.Reason)
	}
}

func TestWatchStartRejectsRunKwarg(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch_start(db, files = ["/data/**"], run = lambda: None)
`)
	if res.Result != "fail" || !strings.Contains(res.Reason, "does not take run=") {
		t.Errorf("watch_start(run=) should be rejected, got: %s", res.Reason)
	}
}

func TestWatchStopWithoutStart(t *testing.T) {
	res := runWatchTest(t, `
def test_w():
    watch_stop(db)
`)
	if res.Result != "fail" || !strings.Contains(res.Reason, "no watch is active") {
		t.Errorf("watch_stop() without a watch should fail clearly, got: %s", res.Reason)
	}
}

func TestWatchAcceptsValidOps(t *testing.T) {
	if err := loadWatchSpec(t, `
def test_w():
    watch(db, files = ["/data/**/*.wal"], ops = ["write", "open", "read", "close"], run = lambda: None)
`); err != nil {
		t.Fatalf("valid watch rejected at load: %v", err)
	}
}

// ─── event routing ─────────────────────────────────────────────────────────

func TestWatchMatchingByPathAndOp(t *testing.T) {
	tests := []struct {
		name  string
		spec  watchSpec
		io    seccheck.FileIO
		match bool
	}{
		{
			"path and op match",
			watchSpec{Files: []string{"/data/**/*.wal"}, Ops: map[string]bool{"write": true}},
			seccheck.FileIO{Op: seccheck.OpWrite, Path: "/data/pg/x.wal"},
			true,
		},
		{
			"op excluded",
			watchSpec{Files: []string{"/data/**"}, Ops: map[string]bool{"write": true}},
			seccheck.FileIO{Op: seccheck.OpRead, Path: "/data/x"},
			false,
		},
		{
			"path excluded",
			watchSpec{Files: []string{"/data/**"}, Ops: map[string]bool{"write": true}},
			seccheck.FileIO{Op: seccheck.OpWrite, Path: "/etc/passwd"},
			false,
		},
		{
			"no ops filter matches every op",
			watchSpec{Files: []string{"/data/**"}},
			seccheck.FileIO{Op: seccheck.OpClose, Path: "/data/x"},
			true,
		},
		{
			"no files filter matches every path",
			watchSpec{Ops: map[string]bool{"write": true}},
			seccheck.FileIO{Op: seccheck.OpWrite, Path: "/anywhere"},
			true,
		},
		{
			"** crosses directories, which is why M3 shipped",
			watchSpec{Files: []string{"/var/lib/**/pg_wal/*"}},
			seccheck.FileIO{Op: seccheck.OpWrite, Path: "/var/lib/postgresql/data/pg_wal/0001"},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.matches(tc.io); got != tc.match {
				t.Errorf("matches(%s %s) = %v, want %v", tc.io.Op, tc.io.Path, got, tc.match)
			}
		})
	}
}

func TestFileIOEventCarriesOffsetCountAndErrno(t *testing.T) {
	rt := New(testLogger())
	rt.watches.add(&watchSpec{Service: "db", Files: []string{"/data/**"}})

	rt.onFileIO("db", seccheck.FileIO{
		Op: seccheck.OpWrite, Path: "/data/pg_wal/0001", FD: 7,
		Count: 8192, Result: 8192, Offset: 7815168, HasOffset: true,
		PID: 42, ProcessName: "postgres",
	})

	evs := rt.events.Events()
	var found bool
	for _, e := range evs {
		if e.Type != "file_io" {
			continue
		}
		found = true
		// offset + count is the capability that does not exist today.
		if e.Fields["offset"] != "7815168" {
			t.Errorf("offset = %q, want 7815168", e.Fields["offset"])
		}
		if e.Fields["count"] != "8192" {
			t.Errorf("count = %q, want 8192", e.Fields["count"])
		}
		if e.Fields["path"] != "/data/pg_wal/0001" {
			t.Errorf("path = %q", e.Fields["path"])
		}
		if e.Fields["process"] != "postgres" {
			t.Errorf("process = %q", e.Fields["process"])
		}
	}
	if !found {
		t.Fatal("no file_io event emitted")
	}
}

func TestFileIOEventFlagsShortWrite(t *testing.T) {
	rt := New(testLogger())
	rt.watches.add(&watchSpec{Service: "db"})

	rt.onFileIO("db", seccheck.FileIO{
		Op: seccheck.OpWrite, Path: "/data/x", Count: 8192, Result: 4096,
	})

	for _, e := range rt.events.Events() {
		if e.Type == "file_io" {
			// A short write is the precondition for a torn record; it must be
			// a field, not something the reader has to derive.
			if e.Fields["short"] != "true" {
				t.Errorf("short write not flagged: %v", e.Fields)
			}
			return
		}
	}
	t.Fatal("no file_io event emitted")
}

func TestFileIOEventCarriesRealErrno(t *testing.T) {
	rt := New(testLogger())
	rt.watches.add(&watchSpec{Service: "db"})

	rt.onFileIO("db", seccheck.FileIO{
		Op: seccheck.OpOpen, Path: "/lib/libpq.so.5", Result: 0, Errno: 2,
	})

	for _, e := range rt.events.Events() {
		if e.Type == "file_io" {
			if e.Fields["errno"] != "2" {
				t.Errorf("errno = %q, want 2 (ENOENT) — the real errno, not an inference", e.Fields["errno"])
			}
			return
		}
	}
	t.Fatal("no file_io event emitted")
}

// TestUnwatchedIOEmitsNothing: a watch scoped to one subtree must not turn
// the whole event log into a firehose of the SUT's dynamic-linker reads.
func TestUnwatchedIOEmitsNothing(t *testing.T) {
	rt := New(testLogger())
	rt.watches.add(&watchSpec{Service: "db", Files: []string{"/data/**"}})

	rt.onFileIO("db", seccheck.FileIO{Op: seccheck.OpRead, Path: "/usr/lib/libc.so"})
	rt.onFileIO("db", seccheck.FileIO{Op: seccheck.OpOpen, Path: "/etc/ld.so.cache"})

	for _, e := range rt.events.Events() {
		if e.Type == "file_io" {
			t.Errorf("unwatched path produced an event: %v", e.Fields)
		}
	}
}

func TestNoWatchMeansNoEvents(t *testing.T) {
	rt := New(testLogger())
	rt.onFileIO("db", seccheck.FileIO{Op: seccheck.OpWrite, Path: "/data/x"})
	for _, e := range rt.events.Events() {
		if e.Type == "file_io" {
			t.Error("file_io emitted with no watch installed")
		}
	}
}
