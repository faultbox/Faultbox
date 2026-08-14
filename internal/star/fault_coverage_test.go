package star

import (
	"testing"

	"github.com/faultbox/Faultbox/internal/engine"
)

// TestUncoveredSyscallsIsWhatMakesTheScanSafe.
//
// Which syscalls get intercepted is decided by a source scan before the
// run. The scan is a heuristic and always will be — it cannot see a fault
// built in a variable or reached through a helper. Without this check
// those faults are accepted, never fire, and the test passes for the
// wrong reason.
//
// This is the guard that turns "the scan might miss it" from a silent
// wrong answer into a loud failure, which is why the manifest row can be
// green while the scan itself stays a heuristic.
func TestUncoveredSyscallsIsWhatMakesTheScanSafe(t *testing.T) {
	sess, err := engine.NewSession(engine.SessionConfig{
		Binary:             "/bin/true",
		ExternalListenerFd: -1,
		FaultRules: []engine.FaultRule{
			{Syscall: "write"},
			{Syscall: "writev"},
			{Syscall: "pwrite64"},
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	t.Run("a covered fault installs", func(t *testing.T) {
		got := uncoveredSyscalls(sess, []engine.FaultRule{{Syscall: "write"}})
		if len(got) != 0 {
			t.Errorf("write reported uncovered by a filter that intercepts it: %v", got)
		}
	})

	t.Run("an uncovered fault is caught", func(t *testing.T) {
		got := uncoveredSyscalls(sess, []engine.FaultRule{{Syscall: "connect"}})
		if len(got) != 1 || got[0] != "connect" {
			t.Errorf("uncoveredSyscalls = %v, want [connect]", got)
		}
	})

	t.Run("mixed rules report only the gaps, sorted", func(t *testing.T) {
		got := uncoveredSyscalls(sess, []engine.FaultRule{
			{Syscall: "write"},   // covered
			{Syscall: "connect"}, // not
			{Syscall: "read"},    // not
			{Syscall: "connect"}, // duplicate of a gap
		})
		if len(got) != 2 || got[0] != "connect" || got[1] != "read" {
			t.Errorf("uncoveredSyscalls = %v, want [connect read]", got)
		}
	})

	t.Run("a nil session is not a coverage gap", func(t *testing.T) {
		// Mocks and seccomp=False services have no session at all; that
		// path is handled separately and must not be turned into a
		// coverage error here.
		if got := uncoveredSyscalls(nil, []engine.FaultRule{{Syscall: "write"}}); got != nil {
			t.Errorf("nil session reported gaps: %v", got)
		}
	})
}

// TestFiltersSyscallMirrorsWhatLaunchInstalls guards the one asymmetry
// between the declared rules and the filter the kernel actually gets:
// launch() adds openat whenever open is requested, because Go programs
// call openat. A check that did not mirror it would reject a working
// fault.
func TestFiltersSyscallMirrorsWhatLaunchInstalls(t *testing.T) {
	sess, err := engine.NewSession(engine.SessionConfig{
		Binary:             "/bin/true",
		ExternalListenerFd: -1,
		FaultRules:         []engine.FaultRule{{Syscall: "open"}},
	}, testLogger())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if !sess.FiltersSyscall("open") {
		t.Error("open is not reported as filtered")
	}
	if !sess.FiltersSyscall("openat") {
		t.Error("openat is not reported as filtered, but launch() installs it alongside open")
	}
	if sess.FiltersSyscall("write") {
		t.Error("write reported as filtered by a filter that only covers open")
	}

	// The listing explains the gap, so it must name both.
	got := sess.FilteredSyscalls()
	if len(got) != 2 || got[0] != "open" || got[1] != "openat" {
		t.Errorf("FilteredSyscalls() = %v, want [open openat]", got)
	}
}
