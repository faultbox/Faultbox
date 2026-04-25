package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewLockHasSchema(t *testing.T) {
	lk := New("0.10.0", time.Now())
	if lk.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %d, want %d", lk.SchemaVersion, SchemaVersion)
	}
	if lk.LockVersion != "0.10.0" {
		t.Errorf("lock_version = %q", lk.LockVersion)
	}
	if lk.GeneratedAt == "" {
		t.Error("generated_at empty")
	}
	if lk.Images == nil {
		t.Error("Images map should be initialised")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faultbox.lock")
	lk := New("0.10.0", time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC))
	lk.Images["mysql:8.0.32"] = "sha256:aaa111"
	lk.Images["redis:7-alpine"] = "sha256:bbb222"
	if err := lk.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Images["mysql:8.0.32"] != "sha256:aaa111" {
		t.Errorf("mysql digest lost in roundtrip: %+v", got.Images)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("schema lost: %d", got.SchemaVersion)
	}
}

func TestReadMissingReturnsNil(t *testing.T) {
	lk, err := Read(filepath.Join(t.TempDir(), "nonexistent.lock"))
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if lk != nil {
		t.Errorf("missing file should return nil lock, got %+v", lk)
	}
}

func TestReadFutureSchemaRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.lock")
	body, _ := json.Marshal(Lock{
		SchemaVersion: SchemaVersion + 99,
		LockVersion:   "999.0.0",
	})
	_ = os.WriteFile(path, body, 0o644)
	_, err := Read(path)
	if err == nil {
		t.Fatal("expected refusal for future schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error should mention schema_version: %v", err)
	}
}

func TestCompareImagesNoDriftIsEmpty(t *testing.T) {
	lk := New("0.10.0", time.Now())
	lk.Images["mysql:8.0.32"] = "sha256:aaa"
	lk.Images["redis:7"] = "sha256:bbb"
	d := lk.CompareImages(map[string]string{
		"mysql:8.0.32": "sha256:aaa",
		"redis:7":      "sha256:bbb",
	})
	if !d.Empty() {
		t.Errorf("expected empty drift; got %+v", d)
	}
}

func TestCompareImagesNewAndChanged(t *testing.T) {
	lk := New("0.10.0", time.Now())
	lk.Images["mysql:8.0.32"] = "sha256:aaa"
	lk.Images["redis:7"] = "sha256:bbb"

	observed := map[string]string{
		"mysql:8.0.32": "sha256:zzz", // changed
		"redis:7":      "sha256:bbb", // unchanged
		"kafka:3.7.0":  "sha256:ccc", // new
	}
	d := lk.CompareImages(observed)
	if d.Empty() {
		t.Fatal("expected drift but got none")
	}
	if len(d.NewImages) != 1 || d.NewImages[0] != "kafka:3.7.0" {
		t.Errorf("NewImages = %v", d.NewImages)
	}
	if ch := d.ChangedImages["mysql:8.0.32"]; ch.From != "sha256:aaa" || ch.To != "sha256:zzz" {
		t.Errorf("ChangedImages = %+v", d.ChangedImages)
	}
}

func TestCompareImagesRemovedFromSpec(t *testing.T) {
	lk := New("0.10.0", time.Now())
	lk.Images["was-there:1"] = "sha256:aaa"
	d := lk.CompareImages(map[string]string{}) // spec no longer references it
	if len(d.RemovedImages) != 1 || d.RemovedImages[0] != "was-there:1" {
		t.Errorf("RemovedImages = %v", d.RemovedImages)
	}
}

// TestFormatRendersActionableTable pins the v0.12 #82 output: a
// "drift detected (N entries):" header followed by one line per
// drifted entry naming the locked vs current digest. This is the
// CI-actionable view inDrive Freight asked for; the prior format
// summarised changes by category which forced an extra round-trip
// to figure out *which* image moved.
func TestFormatRendersActionableTable(t *testing.T) {
	lk := New("0.10.0", time.Now())
	lk.Images["a:1"] = "sha256:aaaa00000000000000000000000000000000000000000000000000000000aaaa"
	lk.Images["was-there:1"] = "sha256:dddd00000000000000000000000000000000000000000000000000000000dddd"
	d := lk.CompareImages(map[string]string{
		"a:1": "sha256:bbbb00000000000000000000000000000000000000000000000000000000bbbb", // changed
		"b:2": "sha256:cccc00000000000000000000000000000000000000000000000000000000cccc", // new
	})
	out := d.Format()

	wants := []string{
		"drift detected (3 entries):",
		"a:1",
		"b:2",
		"was-there:1",
		"locked",
		"current",
		"<not in lock>",
		"<not found locally>",
		"sha256:aaaa00000000", // shortDigest of locked a:1
		"sha256:bbbb00000000", // shortDigest of current a:1
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("Format() missing %q\n--- output ---\n%s", w, out)
		}
	}
}
