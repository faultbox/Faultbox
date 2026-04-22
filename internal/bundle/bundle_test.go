package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readBundle reads a .fb file on disk and returns a name→content map
// of everything inside. Used by every test that asserts on bundle
// contents.
func readBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	buf := mustReadFile(t, path)
	return readBundleBytes(t, buf)
}

func readBundleBytes(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	defer gz.Close()

	out := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = body
	}
	return out
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := readFileAll(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func readFileAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func TestWriterRoundTrip(t *testing.T) {
	w := NewWriter()
	man := Manifest{
		SchemaVersion:   SchemaVersion,
		FaultboxVersion: "0.9.7-test",
		RunID:           "abcdef",
		CreatedAt:       "2026-04-22T15:03:11Z",
		Seed:            42,
		Tests: []TestRow{
			{Name: "test_foo", Outcome: "passed", DurationMs: 123},
		},
		Summary: Summary{Total: 1, Passed: 1},
	}
	if err := w.AddJSON("manifest.json", man); err != nil {
		t.Fatalf("AddJSON manifest: %v", err)
	}
	env := Env{FaultboxVersion: "0.9.7-test", HostOS: "linux"}
	if err := w.AddJSON("env.json", env); err != nil {
		t.Fatalf("AddJSON env: %v", err)
	}
	w.AddFile("replay.sh", GenerateReplayScript("run-test.fb", "0.9.7-test", time.Now()))
	w.AddFile("trace.json", []byte(`{"version":1,"pass":1,"fail":0}`))

	dst := filepath.Join(t.TempDir(), "run-test.fb")
	if err := w.WriteTo(dst); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	contents := readBundle(t, dst)
	for _, want := range []string{"manifest.json", "env.json", "trace.json", "replay.sh"} {
		if _, ok := contents[want]; !ok {
			t.Errorf("bundle missing %s", want)
		}
	}

	var got Manifest
	if err := json.Unmarshal(contents["manifest.json"], &got); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if got.Seed != 42 || got.Tests[0].Name != "test_foo" {
		t.Errorf("manifest roundtrip mismatched: %+v", got)
	}

	// replay.sh should be executable and reference the bundle filename.
	replay := string(contents["replay.sh"])
	if !strings.Contains(replay, "faultbox replay") {
		t.Errorf("replay.sh missing command: %q", replay)
	}
	if !strings.Contains(replay, "run-test.fb") {
		t.Errorf("replay.sh missing bundle path: %q", replay)
	}
}

func TestWriterDeterministic(t *testing.T) {
	// Two bundles built from the same inputs must be byte-identical so
	// downstream digest-based checks (faultbox.lock, diff tooling) are
	// stable. Relies on fixed mtime in writer.encode().
	build := func() []byte {
		w := NewWriter()
		_ = w.AddJSON("manifest.json", Manifest{SchemaVersion: 1, RunID: "x"})
		w.AddFile("trace.json", []byte(`{"k":"v"}`))
		w.AddFile("replay.sh", []byte("#!/bin/sh\nexec faultbox replay run.fb\n"))
		w.AddFile("env.json", []byte(`{"faultbox_version":"0.9.7-test"}`))
		buf, err := w.encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}

	a, b := build(), build()
	if !bytes.Equal(a, b) {
		t.Error("two encode() calls produced different bytes; bundle is nondeterministic")
	}
}

func TestWriterPriorityOrder(t *testing.T) {
	// manifest.json, env.json, trace.json, replay.sh should come first
	// in the archive so `tar -tf` users see the important files up
	// front. Subsequent files land alphabetically.
	w := NewWriter()
	w.AddFile("services/z.stdout", []byte("last"))
	w.AddFile("trace.json", []byte("{}"))
	w.AddFile("replay.sh", []byte("#!/bin/sh\n"))
	w.AddFile("services/a.stdout", []byte("first"))
	w.AddFile("env.json", []byte("{}"))
	_ = w.AddJSON("manifest.json", Manifest{})

	dst := filepath.Join(t.TempDir(), "ordered.fb")
	if err := w.WriteTo(dst); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	var names []string
	gz, _ := gzip.NewReader(bytes.NewReader(mustReadFile(t, dst)))
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, hdr.Name)
	}

	want := []string{"manifest.json", "env.json", "trace.json", "replay.sh", "services/a.stdout", "services/z.stdout"}
	if !equal(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestDefaultFilename(t *testing.T) {
	got := DefaultFilename(time.Date(2026, 4, 22, 15, 3, 11, 0, time.UTC), 42)
	want := "run-2026-04-22T15-03-11-42.fb"
	if got != want {
		t.Errorf("DefaultFilename = %q, want %q", got, want)
	}
}

func TestReplayScriptSafe(t *testing.T) {
	// A filename with shell metacharacters should be sanitised before
	// interpolation. This keeps the script safe even if a test
	// framework somewhere emits an unusual bundle name.
	script := string(GenerateReplayScript("run ; rm -rf /.fb", "0.9.7", time.Now()))
	if strings.Contains(script, "; rm") {
		t.Errorf("replay.sh must sanitise dangerous chars, got: %q", script)
	}
	if !strings.Contains(script, "faultbox replay") {
		t.Errorf("replay.sh missing command")
	}
}

func TestGatherEnvHasHostFields(t *testing.T) {
	env := GatherEnv("0.9.7", "abc123")
	if env.FaultboxVersion != "0.9.7" || env.FaultboxCommit != "abc123" {
		t.Errorf("version fields not preserved: %+v", env)
	}
	if env.HostOS == "" || env.HostArch == "" {
		t.Errorf("host OS/arch should always populate: %+v", env)
	}
	if env.GoToolchain == "" {
		t.Errorf("go toolchain should populate: %+v", env)
	}
}

func TestBuildPopulatesAllCoreFiles(t *testing.T) {
	in := BuildInput{
		FaultboxVersion: "0.9.7-test",
		FaultboxCommit:  "deadbee",
		Seed:            42,
		SpecRoot:        "faultbox.star",
		CreatedAt:       time.Date(2026, 4, 22, 15, 3, 11, 0, time.UTC),
		Tests: []TestRow{
			{Name: "test_a", Outcome: "passed", DurationMs: 10},
			{Name: "test_b", Outcome: "failed", DurationMs: 20},
			{Name: "test_c", Outcome: "errored", DurationMs: 5},
		},
		Trace: []byte(`{"version":1}`),
	}
	w, filename, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if filename != "run-2026-04-22T15-03-11-42.fb" {
		t.Errorf("filename = %q", filename)
	}

	buf, err := w.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	contents := readBundleBytes(t, buf.Bytes())

	var man Manifest
	if err := json.Unmarshal(contents["manifest.json"], &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", man.SchemaVersion, SchemaVersion)
	}
	wantSummary := Summary{Total: 3, Passed: 1, Failed: 1, Errored: 1}
	if man.Summary != wantSummary {
		t.Errorf("summary = %+v, want %+v", man.Summary, wantSummary)
	}
	if man.RunID == "" {
		t.Error("RunID should be non-empty")
	}

	var env Env
	if err := json.Unmarshal(contents["env.json"], &env); err != nil {
		t.Fatalf("env parse: %v", err)
	}
	if env.FaultboxVersion != "0.9.7-test" || env.FaultboxCommit != "deadbee" {
		t.Errorf("env version mismatched: %+v", env)
	}

	if !strings.Contains(string(contents["replay.sh"]), "run-2026-04-22T15-03-11-42.fb") {
		t.Errorf("replay.sh missing filename: %q", contents["replay.sh"])
	}
	if !bytes.Contains(contents["trace.json"], []byte(`"version":1`)) {
		t.Errorf("trace.json missing content: %q", contents["trace.json"])
	}
}

func TestBuildCapturesSpecFiles(t *testing.T) {
	// Phase 4: every local .star file the runtime loaded should land
	// under spec/<relative-path> in the archive so a bundle is enough
	// to reproduce the run source-code-wise.
	in := BuildInput{
		FaultboxVersion: "0.9.7-test",
		Seed:            1,
		CreatedAt:       time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		Trace:           []byte(`{}`),
		Specs: map[string][]byte{
			"faultbox.star":       []byte("load('helpers/jwt.star', 'sign')\n"),
			"helpers/jwt.star":    []byte("def sign(): return 'ok'\n"),
			"_external/weird.star": []byte("# outside baseDir, captured defensively\n"),
		},
	}
	w, _, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	buf, err := w.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	files := readBundleBytes(t, buf.Bytes())
	wants := []string{
		"spec/faultbox.star",
		"spec/helpers/jwt.star",
		"spec/_external/weird.star",
	}
	for _, p := range wants {
		if _, ok := files[p]; !ok {
			t.Errorf("bundle missing %q", p)
		}
	}
	// Content should round-trip verbatim — no normalisation.
	if !bytes.Contains(files["spec/helpers/jwt.star"], []byte("def sign()")) {
		t.Errorf("spec/helpers/jwt.star body not preserved: %q", files["spec/helpers/jwt.star"])
	}
}

func TestBuildOmitsSpecSectionWhenEmpty(t *testing.T) {
	// Callers that pass no specs (older test fixtures, stripped-down
	// invocations) should still produce a valid archive — just without
	// a spec/ section.
	in := BuildInput{
		FaultboxVersion: "0.9.7-test",
		CreatedAt:       time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		Trace:           []byte(`{}`),
	}
	w, _, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	buf, _ := w.encode()
	files := readBundleBytes(t, buf.Bytes())
	for name := range files {
		if strings.HasPrefix(name, "spec/") {
			t.Errorf("unexpected spec entry %q when Specs was nil", name)
		}
	}
}

func TestResolvePath(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		envDir   string
		filename string
		want     string
	}{
		{"explicit wins", "/x/y.fb", "/tmp", "default.fb", "/x/y.fb"},
		{"env dir used when no explicit", "", "/tmp", "run.fb", "/tmp/run.fb"},
		{"default to cwd", "", "", "run.fb", "run.fb"},
	}
	for _, c := range cases {
		got := ResolvePath(c.explicit, c.envDir, c.filename)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
