package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestBundle writes a minimal valid .fb archive to a temp path and
// returns that path. Used as setup across reader tests.
func newTestBundle(t *testing.T, man Manifest) string {
	t.Helper()
	if man.SchemaVersion == 0 {
		man.SchemaVersion = SchemaVersion
	}
	w := NewWriter()
	if err := w.AddJSON("manifest.json", man); err != nil {
		t.Fatalf("AddJSON manifest: %v", err)
	}
	if err := w.AddJSON("env.json", Env{FaultboxVersion: man.FaultboxVersion}); err != nil {
		t.Fatalf("AddJSON env: %v", err)
	}
	w.AddFile("trace.json", []byte(`{"version":1}`))
	w.AddFile("replay.sh", GenerateReplayScript("run-test.fb", man.FaultboxVersion, time.Now()))
	path := filepath.Join(t.TempDir(), "run-test.fb")
	if err := w.WriteTo(path); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return path
}

func TestOpenRoundTrip(t *testing.T) {
	path := newTestBundle(t, Manifest{
		FaultboxVersion: "0.9.7-test",
		RunID:           "abc",
		Seed:            42,
		Tests: []TestRow{
			{Name: "test_foo", Outcome: "passed", DurationMs: 10},
		},
		Summary: Summary{Total: 1, Passed: 1},
	})

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r.Manifest().Seed != 42 {
		t.Errorf("manifest seed = %d, want 42", r.Manifest().Seed)
	}
	if r.Env().FaultboxVersion != "0.9.7-test" {
		t.Errorf("env version = %q", r.Env().FaultboxVersion)
	}
	files := r.Files()
	want := []string{"manifest.json", "env.json", "trace.json", "replay.sh"}
	if !equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}

	trace, err := r.File("trace.json")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !bytes.Contains(trace, []byte(`"version":1`)) {
		t.Errorf("trace.json content unexpected: %q", trace)
	}

	if _, err := r.File("nonexistent"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	// Produce an archive that claims a future schema version.
	// The reader must refuse — mis-parsing silently would be worse
	// than an explicit "upgrade faultbox" error.
	path := newTestBundle(t, Manifest{
		SchemaVersion:   SchemaVersion + 1,
		FaultboxVersion: "99.0.0",
	})
	_, err := Open(path)
	if err == nil {
		t.Fatal("expected refusal for newer schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error message should mention schema_version, got: %v", err)
	}
}

func TestOpenRefusesMissingManifest(t *testing.T) {
	// An archive without manifest.json is not a valid .fb bundle.
	path := filepath.Join(t.TempDir(), "naked.fb")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "something.txt", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("abc"))
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected refusal when manifest.json is absent")
	}
}

func TestExtractAllFiles(t *testing.T) {
	path := newTestBundle(t, Manifest{FaultboxVersion: "0.9.7-test"})
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	n, err := r.Extract(dst)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if n != 4 {
		t.Errorf("extracted %d files, want 4", n)
	}
	for _, name := range []string{"manifest.json", "env.json", "trace.json", "replay.sh"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("missing %s after extract: %v", name, err)
		}
	}
	// replay.sh must be executable; otherwise users get a confusing
	// "Permission denied" when they try to run it.
	info, _ := os.Stat(filepath.Join(dst, "replay.sh"))
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("replay.sh not executable after extract: %v", info.Mode())
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	// Hand-craft an archive with a relative-escape path. Open() stages
	// it in memory; Extract must refuse to write outside dst.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	man := Manifest{SchemaVersion: SchemaVersion, FaultboxVersion: "0.9.7"}
	manBytes, _ := json.Marshal(man)
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manBytes))})
	_, _ = tw.Write(manBytes)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("pwn"))
	_ = tw.Close()
	_ = gz.Close()

	path := filepath.Join(t.TempDir(), "evil.fb")
	_ = os.WriteFile(path, buf.Bytes(), 0o644)

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := r.Extract(dst); err == nil {
		t.Error("expected Extract to refuse path-traversal entry")
	}
}

func TestCheckVersionMatrix(t *testing.T) {
	cases := []struct {
		bundle, current string
		want            VersionMismatchKind
	}{
		{"0.9.7", "0.9.7", VersionSame},
		{"0.9.7", "0.9.8", VersionMinorPatchDrift},
		{"0.9.7", "0.10.0", VersionMinorPatchDrift},
		{"0.9.7", "1.0.0", VersionMajorDrift},
		{"1.2.3", "0.9.7", VersionMajorDrift},
		{"0.9.7", "", VersionUnknown},
		{"", "0.9.7", VersionUnknown},
		{"custom", "0.9.7", VersionUnknown},
		{"v0.9.7", "0.9.7", VersionSame},
	}
	for _, c := range cases {
		got := CheckVersion(c.bundle, c.current)
		if got.Kind != c.want {
			t.Errorf("CheckVersion(%q, %q) = %v, want %v",
				c.bundle, c.current, got.Kind, c.want)
		}
	}
}
