package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestDeny_RejectsUnknownErrno guards #139: an errno name Faultbox can't
// inject must fail at spec load, not silently apply as errno 0 (a deny that
// denies nothing).
func TestDeny_RejectsUnknownErrno(t *testing.T) {
	thread := &starlark.Thread{}
	deny := func(errno string) (starlark.Value, error) {
		return builtinDeny(thread, nil, starlark.Tuple{starlark.String(errno)}, nil)
	}

	// A supported errno loads.
	if _, err := deny("EIO"); err != nil {
		t.Fatalf("deny(\"EIO\") should be valid: %v", err)
	}
	// Case-insensitive.
	if _, err := deny("eio"); err != nil {
		t.Errorf("deny(\"eio\") should be valid (case-insensitive): %v", err)
	}

	// An unknown errno is rejected with a helpful message.
	_, err := deny("EDEADLK")
	if err == nil {
		t.Fatal("deny(\"EDEADLK\") must error at spec load, got nil (#139)")
	}
	if !strings.Contains(err.Error(), "unknown errno") {
		t.Errorf("error = %q, want it to mention 'unknown errno'", err.Error())
	}
	// The message lists a supported errno so the user can self-correct.
	if !strings.Contains(err.Error(), "EIO") {
		t.Errorf("error = %q, want it to list supported errnos", err.Error())
	}
}
