package star

import (
	"strings"
	"testing"
)

// TestService_RejectsUnknownKwarg guards #140: an unknown service() kwarg must
// fail at spec load. Previously a typo (imagee=) or a roadmap kwarg
// (determinism="L4") was silently dropped, so the author's intent vanished
// with no signal.
func TestService_RejectsUnknownKwarg(t *testing.T) {
	err := New(testLogger()).LoadString("spec.star",
		`svc = service("s", image="busybox", determinism="L4")`)
	if err == nil {
		t.Fatal("service() with an unknown kwarg must error at spec load (#140), got nil")
	}
	if !strings.Contains(err.Error(), "unknown keyword argument") || !strings.Contains(err.Error(), "determinism") {
		t.Errorf("error = %q, want it to name the unknown kwarg 'determinism'", err.Error())
	}

	// A typo is caught too.
	if err := New(testLogger()).LoadString("spec.star",
		`svc = service("s", image="busybox", imagee="x")`); err == nil {
		t.Error("service() with a typo'd kwarg must error at spec load")
	}

	// Valid kwargs still load.
	if err := New(testLogger()).LoadString("spec.star",
		`svc = service("s", image="busybox", reuse=True, seed=lambda: None)`); err != nil {
		t.Errorf("valid service() kwargs should load: %v", err)
	}
}
