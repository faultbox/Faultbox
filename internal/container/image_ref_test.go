package container

import (
	"errors"
	"strings"
	"testing"
)

// TestIsLocalOnlyRef pins the fast-fail heuristic, and — more
// importantly — pins how narrow it is. Refusing to pull an image that
// would have worked is worse than the two minutes it saves, so the
// negative cases carry the weight here.
func TestIsLocalOnlyRef(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"courier-faultbox-mysql:local", true},
		{"myapp:local", true},
		{"registry.example.com/team/app:local", true},

		// Everything below must still be attempted.
		{"mysql:8", false},            // no registry host, pulls fine
		{"postgres:16", false},        // ditto
		{"alpine", false},             // no tag at all
		{"redis:latest", false},       // explicit latest
		{"myapp:localdev", false},     // not the :local tag
		{"myapp:local-1", false},      // ditto
		{"localhost:5000/app", false}, // registry port, not a tag
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			if got := isLocalOnlyRef(tt.ref); got != tt.want {
				t.Errorf("isLocalOnlyRef(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

// TestDescribePullFailureExplainsDenied covers the case the heuristic
// deliberately does not catch. Docker reports a pull for an image that
// was never pushed as an authorization failure, which sends people
// looking for credentials they do not need.
func TestDescribePullFailureExplainsDenied(t *testing.T) {
	orig := errors.New("denied: requested access to the resource is denied")
	got := describePullFailure("courier-mysql:v2", orig)

	if !errors.Is(got, orig) {
		t.Error("describePullFailure dropped the underlying error")
	}
	for _, want := range []string{"built locally", "docker build", "courier-mysql:v2"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("message %q does not mention %q", got.Error(), want)
		}
	}
}

// TestDescribePullFailureLeavesUnrelatedErrorsAlone guards against
// attaching a misleading hint to a failure that has nothing to do with
// local builds — the exact mistake F-5 is about.
func TestDescribePullFailureLeavesUnrelatedErrorsAlone(t *testing.T) {
	orig := errors.New("dial tcp: lookup registry.example.com: no such host")
	got := describePullFailure("registry.example.com/app:1", orig)

	if got.Error() != orig.Error() {
		t.Errorf("a network error was annotated with a local-build hint: %q", got.Error())
	}
}
