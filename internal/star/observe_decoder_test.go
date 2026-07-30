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

// RFC-052 M5 — the removals.
//
// These names were deprecated in v0.13.0 with a warning naming v0.14.0 as the
// removal version, then shipped in five further releases. Anyone who read the
// warning carefully concluded the removal had already happened.
//
// They are removed in v0.17.0, but the names stay registered as failing stubs:
// deleting them outright gives "undefined: stdout", which is true and useless.
// A spec written against the old API — or an agent working from documentation
// that predates the change — deserves to be told where the thing went.
func TestRemovedBuiltinsFailLegibly(t *testing.T) {
	cases := []struct {
		src         string
		name        string
		replacement string
	}{
		{`x = stdout()`, "stdout", "observe.stdout"},
		{`x = stderr()`, "stderr", "observe.stderr"},
		{`x = json_decoder()`, "json_decoder", `decoder("json")`},
		{`x = logfmt_decoder()`, "logfmt_decoder", `decoder("logfmt")`},
		{`x = regex_decoder(pattern="x")`, "regex_decoder", `decoder("regex"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := New(testLogger())
			err := rt.LoadString("spec.star", tc.src)
			if err == nil {
				t.Fatalf("%s() still works — it was supposed to be removed", tc.name)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.name) {
				t.Errorf("error does not name the removed builtin: %q", msg)
			}
			if !strings.Contains(msg, tc.replacement) {
				t.Errorf("error does not name the replacement %q: %q", tc.replacement, msg)
			}
			if !strings.Contains(msg, "removed in v0.17.0") {
				t.Errorf("error does not say when it was removed: %q", msg)
			}
			// "undefined: stdout" is what deleting the name would give. If we
			// ever get that, the stub has been dropped and the message with it.
			if strings.Contains(msg, "undefined:") {
				t.Errorf("removal produced a bare undefined error: %q", msg)
			}
		})
	}
}

// The replacements must, of course, work.
func TestReplacementsForRemovedBuiltinsWork(t *testing.T) {
	rt := New(testLogger())
	src := `
o = observe.stdout()
e = observe.stderr()
j = decoder("json")
l = decoder("logfmt")
r = decoder("regex", pattern = "x")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("the documented replacements must load: %v", err)
	}
	for _, n := range []string{"o", "e"} {
		if _, ok := rt.globals[n].(*ObserveSourceVal); !ok {
			t.Errorf("%s = %T, want *ObserveSourceVal", n, rt.globals[n])
		}
	}
	for _, n := range []string{"j", "l", "r"} {
		if _, ok := rt.globals[n].(*DecoderVal); !ok {
			t.Errorf("%s = %T, want *DecoderVal", n, rt.globals[n])
		}
	}
}
