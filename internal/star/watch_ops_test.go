package star

import (
	"strings"
	"testing"
)

// The default install: opens and the write family, no read/close/connect.
var defaultInstalled = []string{
	"syscall/openat/exit",
	"syscall/pwrite64/exit",
	"syscall/write/exit",
	"syscall/writev/exit",
}

// The case that motivated this file. watchableOps accepts "read", the default
// host config does not send it, and nothing previously compared the two — so
// the watch installed cleanly, received nothing, and passed. That is a vacuous
// green wearing the appearance of a working feature.
func TestUndeliverableOpIsRejected(t *testing.T) {
	err := checkOpsDeliverable([]string{"read"}, defaultInstalled)
	if err == nil {
		t.Fatal("watch(ops=[\"read\"]) against a default install must be rejected")
	}
	for _, want := range []string{
		"observe nothing", // says what would happen
		"setup-trace",     // says how to fix it
		"--with-read",     // says exactly which flag
		"dropped points",  // says why it is off by default
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

func TestDeliverableOpsPass(t *testing.T) {
	for _, ops := range [][]string{
		{"open"},
		{"write"},
		{"open", "write"},
		nil, // no ops= means "whatever the session sends"
	} {
		if err := checkOpsDeliverable(ops, defaultInstalled); err != nil {
			t.Errorf("ops=%v should be deliverable by the default install: %v", ops, err)
		}
	}
}

// write must be satisfied by ANY of the write-family points. A session with
// only pwrite64 still delivers writes, and rejecting it would be wrong.
func TestWriteIsSatisfiedByAnyFamilyMember(t *testing.T) {
	for _, installed := range [][]string{
		{"syscall/write/exit"},
		{"syscall/pwrite64/exit"},
		{"syscall/writev/exit"},
	} {
		if err := checkOpsDeliverable([]string{"write"}, installed); err != nil {
			t.Errorf("write should be deliverable by %v: %v", installed, err)
		}
	}
}

func TestExtraPointsMakeOpsDeliverable(t *testing.T) {
	withRead := append(append([]string{}, defaultInstalled...),
		"syscall/read/exit", "syscall/pread64/exit")
	if err := checkOpsDeliverable([]string{"read"}, withRead); err != nil {
		t.Errorf("read should be deliverable after --with-read: %v", err)
	}
	withClose := append(append([]string{}, defaultInstalled...), "syscall/close/exit")
	if err := checkOpsDeliverable([]string{"close"}, withClose); err != nil {
		t.Errorf("close should be deliverable after --with-close: %v", err)
	}
}

// Several missing ops should produce one error naming all of them and all the
// flags — not the first one, leaving the user to discover the rest by
// re-running.
func TestAllMissingOpsAreNamedAtOnce(t *testing.T) {
	err := checkOpsDeliverable([]string{"read", "close", "write"}, defaultInstalled)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"close", "read", "--with-read", "--with-close"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should name %q: %s", want, msg)
		}
	}
	// write IS deliverable and must not be blamed.
	if strings.Contains(msg, "does not send close, read, write") {
		t.Errorf("a deliverable op was reported as missing: %s", msg)
	}
}

// No registration is a different failure with a different remedy, reported by
// the caller. This function must not turn it into "every op is undeliverable".
func TestNoInstalledPointsIsNotAnOpProblem(t *testing.T) {
	if err := checkOpsDeliverable([]string{"read", "close"}, nil); err != nil {
		t.Errorf("an unregistered host is reported elsewhere; got: %v", err)
	}
}

// An op outside the known map is validated by watchableOps/untraceableOps.
// Double-reporting it here would produce two errors for one mistake.
func TestUnknownOpIsLeftToTheOtherValidator(t *testing.T) {
	if err := checkOpsDeliverable([]string{"fsync"}, defaultInstalled); err != nil {
		t.Errorf("fsync is rejected by untraceableOps, not here; got: %v", err)
	}
}
