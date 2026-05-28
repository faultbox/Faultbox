package star

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestObserve_ModuleExposesStdoutAndStderr — RFC-044 §8.6: the
// `observe` Starlark struct exposes stdout/stderr as attributes;
// `observe.stdout()` produces a value of the same type as the
// legacy `stdout()` builtin.
func TestObserve_ModuleExposesStdoutAndStderr(t *testing.T) {
	rt := New(testLogger())
	src := `
out = observe.stdout()
err = observe.stderr()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if _, ok := rt.globals["out"].(*ObserveSourceVal); !ok {
		t.Errorf("observe.stdout() returned %T, want *ObserveSourceVal", rt.globals["out"])
	}
	if _, ok := rt.globals["err"].(*ObserveSourceVal); !ok {
		t.Errorf("observe.stderr() returned %T, want *ObserveSourceVal", rt.globals["err"])
	}
}

// TestObserve_StdoutDeprecatedAliasStillWorks — legacy
// `stdout()` keeps working and produces the same value type as
// `observe.stdout()`. The deprecation warning is asserted
// separately in TestObserve_DeprecationWarning.
func TestObserve_StdoutDeprecatedAliasStillWorks(t *testing.T) {
	resetDeprecationWarnings()
	rt := New(testLogger())
	src := `legacy = stdout()`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if _, ok := rt.globals["legacy"].(*ObserveSourceVal); !ok {
		t.Errorf("legacy stdout() returned %T, want *ObserveSourceVal", rt.globals["legacy"])
	}
}

// TestDecoder_UnifiedDispatcher — RFC-044 §8.7: `decoder("json")`,
// `decoder("logfmt")`, and `decoder("regex", pattern=...)` produce
// DecoderVal values matching the legacy builtins.
func TestDecoder_UnifiedDispatcher(t *testing.T) {
	rt := New(testLogger())
	src := `
j = decoder("json")
l = decoder("logfmt")
r = decoder("regex", pattern="^foo (?P<bar>.+)$")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if got := rt.globals["j"].(*DecoderVal); got.Name != "json" {
		t.Errorf("decoder(\"json\") name = %q, want json", got.Name)
	}
	if got := rt.globals["l"].(*DecoderVal); got.Name != "logfmt" {
		t.Errorf("decoder(\"logfmt\") name = %q, want logfmt", got.Name)
	}
	r := rt.globals["r"].(*DecoderVal)
	if r.Name != "regex" {
		t.Errorf("decoder(\"regex\") name = %q, want regex", r.Name)
	}
	if r.Params["pattern"] != "^foo (?P<bar>.+)$" {
		t.Errorf("decoder(\"regex\") pattern = %q, want ^foo …", r.Params["pattern"])
	}
}

// TestDecoder_Rejections — bad inputs surface clear errors at
// spec load.
func TestDecoder_Rejections(t *testing.T) {
	cases := []struct {
		src      string
		wantSubs string
	}{
		{`d = decoder()`, "exactly one positional argument"},
		{`d = decoder("json", "extra")`, "exactly one positional argument"},
		{`d = decoder(42)`, "must be a string"},
		{`d = decoder("unknown")`, "unknown decoder"},
		{`d = decoder("regex")`, "requires pattern="},
		{`d = decoder("json", pattern="x")`, "no kwargs"},
		{`d = decoder("logfmt", pattern="x")`, "no kwargs"},
	}
	for _, tc := range cases {
		rt := New(testLogger())
		err := rt.LoadString("spec.star", tc.src)
		if err == nil {
			t.Errorf("expected error for %q", tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubs) {
			t.Errorf("for %q, want error containing %q, got %v", tc.src, tc.wantSubs, err)
		}
	}
}

// TestDecoder_LegacyAliasesStillWork — the three legacy
// builtins (json_decoder/logfmt_decoder/regex_decoder) keep
// working and produce identical values to the new dispatcher.
func TestDecoder_LegacyAliasesStillWork(t *testing.T) {
	resetDeprecationWarnings()
	rt := New(testLogger())
	src := `
j = json_decoder()
l = logfmt_decoder()
r = regex_decoder(pattern="x")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	for _, name := range []string{"j", "l", "r"} {
		if _, ok := rt.globals[name].(*DecoderVal); !ok {
			t.Errorf("%s returned %T, want *DecoderVal", name, rt.globals[name])
		}
	}
}

// TestObserve_DeprecationWarning — calling a deprecated builtin
// emits a one-time stderr line naming the replacement. A second
// call doesn't re-warn.
func TestObserve_DeprecationWarning(t *testing.T) {
	resetDeprecationWarnings()
	captured := captureStderr(t, func() {
		rt := New(testLogger())
		src := `
a = stdout()
b = stdout()  # second call must not re-warn
c = stderr()
`
		if err := rt.LoadString("spec.star", src); err != nil {
			t.Fatalf("LoadString: %v", err)
		}
	})
	// Expect exactly one warning for stdout and one for stderr.
	if got := strings.Count(captured, "stdout() is deprecated"); got != 1 {
		t.Errorf("stdout deprecation warnings = %d, want 1; stderr capture:\n%s", got, captured)
	}
	if got := strings.Count(captured, "stderr() is deprecated"); got != 1 {
		t.Errorf("stderr deprecation warnings = %d, want 1; stderr capture:\n%s", got, captured)
	}
	if !strings.Contains(captured, "observe.stdout") {
		t.Errorf("warning should name the replacement (observe.stdout); got:\n%s", captured)
	}
}

// TestDecoder_DeprecationWarning — same one-time semantics for
// the three legacy decoder builtins.
func TestDecoder_DeprecationWarning(t *testing.T) {
	resetDeprecationWarnings()
	captured := captureStderr(t, func() {
		rt := New(testLogger())
		src := `
a = json_decoder()
b = logfmt_decoder()
c = regex_decoder(pattern="x")
`
		if err := rt.LoadString("spec.star", src); err != nil {
			t.Fatalf("LoadString: %v", err)
		}
	})
	for _, name := range []string{"json_decoder", "logfmt_decoder", "regex_decoder"} {
		if got := strings.Count(captured, name+"() is deprecated"); got != 1 {
			t.Errorf("%s warnings = %d, want 1; capture:\n%s", name, got, captured)
		}
	}
	if !strings.Contains(captured, `decoder("json")`) {
		t.Errorf("warning should name replacement decoder(\"json\"); got:\n%s", captured)
	}
}

// captureStderr redirects os.Stderr around fn and returns
// everything written during the call. The actual logger output
// from the runtime is unaffected because testLogger() writes via
// slog to io.Discard.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	defer func() {
		os.Stderr = old
	}()
	fn()
	_ = w.Close()
	<-done
	return buf.String()
}
