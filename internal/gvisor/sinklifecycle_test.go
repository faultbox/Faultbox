package gvisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run must read the endpoint the HOST was configured with, not invent one.
// A self-chosen path would produce a sink nothing ever connects to, and the
// run would report assertions that observed nothing.
func TestSinkEndpointComesFromInstalledConfig(t *testing.T) {
	o := testOpts(t)
	o.SinkPath = "/run/faultbox/custom.sock"
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}
	got, err := SinkEndpoint(o.TraceConfig)
	if err != nil {
		t.Fatalf("SinkEndpoint: %v", err)
	}
	if got != "/run/faultbox/custom.sock" {
		t.Errorf("endpoint = %q, want the configured path", got)
	}
}

// The three broken states must be told apart, because their remedies differ.
func TestSinkEndpointErrorsAreDistinguishable(t *testing.T) {
	dir := t.TempDir()

	t.Run("not registered", func(t *testing.T) {
		_, err := SinkEndpoint(filepath.Join(dir, "absent.json"))
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"not registered", "setup-trace"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		p := filepath.Join(dir, "bad.json")
		_ = os.WriteFile(p, []byte("{not json"), 0o644)
		_, err := SinkEndpoint(p)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "valid JSON") {
			t.Errorf("error should name the parse problem: %v", err)
		}
	})

	t.Run("no sink declared", func(t *testing.T) {
		p := filepath.Join(dir, "nosink.json")
		_ = os.WriteFile(p, []byte(`{"trace_session":{"name":"Default","points":[],"sinks":[]}}`), 0o644)
		_, err := SinkEndpoint(p)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no sink") {
			t.Errorf("error should say there is no sink: %v", err)
		}
	})
}

// A run with no registration must stop, not continue sinkless. Continuing
// would produce watch() assertions that passed having observed nothing — the
// exact vacuous green v0.14.0 withdrew the feature to avoid.
func TestAcquireSinkRefusesWithoutRegistration(t *testing.T) {
	_, err := AcquireSink(filepath.Join(t.TempDir(), "absent.json"), nil, nil)
	if err == nil {
		t.Fatal("AcquireSink must fail when the host is not registered")
	}
	if !strings.Contains(err.Error(), "setup-trace") {
		t.Errorf("error should name the fix: %v", err)
	}
}
