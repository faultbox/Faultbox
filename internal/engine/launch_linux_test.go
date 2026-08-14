//go:build linux

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/faultbox/Faultbox/internal/seccomp"
)

// TestErrnoClassification pins the distinction that F-1 turned on.
//
// `SECCOMP_IOCTL_NOTIF_RECV` returns ENOENT when the notification has
// been discarded — the notifying thread died or took a signal before the
// supervisor got to it. That is a per-notification transient. The
// listener is fine.
//
// It used to be classified as a closed fd, so the notification loop
// returned nil on the first one and stopped supervising for good. The
// target kept its seccomp filter, so every intercepted syscall after
// that blocked forever, and no log line was emitted because a nil error
// reads as a clean shutdown. A Go SUT under SIGURG preemption with a
// busy connection pool reaches that state within seconds.
func TestErrnoClassification(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantClosedFd  bool
		wantDropped   bool
		wantBenignRsp bool
	}{
		{
			name:          "ENOENT from RECV is a dropped notification, not a closed fd",
			err:           fmt.Errorf("ioctl(SECCOMP_IOCTL_NOTIF_RECV): %w", syscall.ENOENT),
			wantClosedFd:  false,
			wantDropped:   true,
			wantBenignRsp: true,
		},
		{
			name:          "ENOENT from SEND is also a dropped notification",
			err:           fmt.Errorf("ioctl(SECCOMP_IOCTL_NOTIF_SEND): %w", syscall.ENOENT),
			wantClosedFd:  false,
			wantDropped:   true,
			wantBenignRsp: true,
		},
		{
			name:          "EBADF is a genuinely closed fd",
			err:           fmt.Errorf("receive notification: %w", syscall.EBADF),
			wantClosedFd:  true,
			wantDropped:   false,
			wantBenignRsp: true,
		},
		{
			name:          "Poll's listener-closed sentinel ends the loop",
			err:           fmt.Errorf("%w (revents=0x10)", seccomp.ErrListenerClosed),
			wantClosedFd:  true,
			wantDropped:   false,
			wantBenignRsp: true,
		},
		{
			name:          "EPIPE ends the loop",
			err:           fmt.Errorf("poll listener: %w", syscall.EPIPE),
			wantClosedFd:  true,
			wantDropped:   false,
			wantBenignRsp: true,
		},
		{
			// fs.ErrNotExist is NOT an fd-closed condition, and treating
			// it as one is how the bug would come back: syscall.Errno.Is
			// maps ENOENT onto fs.ErrNotExist, so any classifier that
			// matches the latter also swallows a dropped notification.
			name:          "fs.ErrNotExist does not end the loop",
			err:           fmt.Errorf("stat: %w", os.ErrNotExist),
			wantClosedFd:  false,
			wantDropped:   false,
			wantBenignRsp: false,
		},
		{
			name:          "an unrelated errno is neither, and must surface",
			err:           fmt.Errorf("receive notification: %w", syscall.EINVAL),
			wantClosedFd:  false,
			wantDropped:   false,
			wantBenignRsp: false,
		},
		{
			name:          "nil is neither",
			err:           nil,
			wantClosedFd:  false,
			wantDropped:   false,
			wantBenignRsp: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClosedFdErr(tt.err); got != tt.wantClosedFd {
				t.Errorf("isClosedFdErr(%v) = %v, want %v", tt.err, got, tt.wantClosedFd)
			}
			if got := isDroppedNotifErr(tt.err); got != tt.wantDropped {
				t.Errorf("isDroppedNotifErr(%v) = %v, want %v", tt.err, got, tt.wantDropped)
			}
			if got := isBenignRespondErr(tt.err); got != tt.wantBenignRsp {
				t.Errorf("isBenignRespondErr(%v) = %v, want %v", tt.err, got, tt.wantBenignRsp)
			}
		})
	}
}

// TestClassificationIsByErrnoNotMessage guards the reason the original bug
// was possible: the classifier matched errno *text*, so "no such file or
// directory" appearing anywhere in a wrapped message was enough to end the
// supervisor. Identity matching means a message that merely mentions the
// phrase is not mistaken for the errno.
func TestClassificationIsByErrnoNotMessage(t *testing.T) {
	// Carries ENOENT's text, but is not ENOENT.
	err := fmt.Errorf("open /etc/faultbox.conf: no such file or directory")

	if isDroppedNotifErr(err) {
		t.Error("a message containing ENOENT's text was mistaken for the errno")
	}
	if isClosedFdErr(err) {
		t.Error("a message containing ENOENT's text was mistaken for a closed fd")
	}
}

// TestENOENTIsNeverAClosedFd is the regression gate for F-1, stated as
// directly as it can be: the exact error `seccomp.Receive` produces when
// a notification is dropped must not end the supervisor.
//
// It also pins the trap that makes this easy to undo. `errors.Is` against
// fs.ErrNotExist matches a wrapped ENOENT, because syscall.Errno.Is maps
// the two together — so a well-meaning "modernize os.IsNotExist" edit
// would reintroduce the bug with no test failing unless this one exists.
func TestENOENTIsNeverAClosedFd(t *testing.T) {
	recvENOENT := fmt.Errorf("ioctl(SECCOMP_IOCTL_NOTIF_RECV): %w", syscall.ENOENT)

	if isClosedFdErr(recvENOENT) {
		t.Fatal("a dropped notification would end the supervisor: " +
			"the target keeps its filter and every intercepted syscall blocks forever")
	}
	if !isDroppedNotifErr(recvENOENT) {
		t.Error("a dropped notification was not recognised as one")
	}

	// The trap, asserted explicitly so it cannot creep back in.
	if !errors.Is(recvENOENT, fs.ErrNotExist) {
		t.Skip("Go no longer maps ENOENT onto fs.ErrNotExist; the guard below is moot")
	}
	if isClosedFdErr(fmt.Errorf("wrapped: %w", fs.ErrNotExist)) {
		t.Error("classifier matches fs.ErrNotExist, which also matches every wrapped ENOENT")
	}
}

// TestSupervisorFailureIsStickyAndDescribed asserts the failure survives to
// the verdict and explains itself. A run whose supervisor died must not be
// reported as a timeout: the reason has to name the actual cause, or the
// next person debugs the SUT instead of Faultbox.
func TestSupervisorFailureIsStickyAndDescribed(t *testing.T) {
	s := &Session{}

	if err := s.SupervisorFailure(); err != nil {
		t.Fatalf("fresh session reported a supervisor failure: %v", err)
	}

	s.noteSupervisorExit("ioctl(SECCOMP_IOCTL_NOTIF_RECV): no such file or directory")

	err := s.SupervisorFailure()
	if err == nil {
		t.Fatal("supervisor failure was not recorded")
	}
	msg := err.Error()
	for _, want := range []string{
		"still running",     // says the target outlived its supervisor
		"blocks indefinite", // says what that costs
		"NOTIF_RECV",        // carries the underlying cause
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("supervisor error %q does not mention %q", msg, want)
		}
	}

	// Sticky: a second read returns the same failure.
	if s.SupervisorFailure() == nil {
		t.Error("supervisor failure did not persist across reads")
	}
}

// TestDroppedNotificationsAreCounted asserts dropped notifications are
// tallied rather than silently swallowed. Individually they are harmless;
// a large count next to strange behaviour is a lead worth having.
func TestDroppedNotificationsAreCounted(t *testing.T) {
	s := &Session{}
	if got := s.DroppedNotifications(); got != 0 {
		t.Fatalf("fresh session has %d dropped notifications, want 0", got)
	}
	s.droppedNotifs.Add(3)
	if got := s.DroppedNotifications(); got != 3 {
		t.Errorf("DroppedNotifications() = %d, want 3", got)
	}
}

// TestStopRequested distinguishes an expected teardown exit from a
// supervisor failure — the whole basis for deciding whether an ended loop
// deserves an error.
func TestStopRequested(t *testing.T) {
	stop := make(chan struct{})
	if stopRequested(stop) {
		t.Error("stopRequested was true before stop was closed")
	}
	close(stop)
	if !stopRequested(stop) {
		t.Error("stopRequested was false after stop was closed")
	}
}

// TestProcessAliveOnSelfAndOnMissingPID sanity-checks the liveness probe
// the early-exit detector depends on. A wrong answer here either misses
// real failures or invents them at every clean teardown.
func TestProcessAliveOnSelfAndOnMissingPID(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive said the test process is not alive")
	}
	// PID 0 is never a real process in /proc.
	if processAlive(0) {
		t.Error("processAlive said PID 0 is alive")
	}
}
