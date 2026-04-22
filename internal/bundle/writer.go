package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Writer assembles a `.fb` archive in memory and writes it to disk.
// Small bundles (KB–MB) fit comfortably; callers that anticipate very
// large per-service logs in a future phase should switch to a streaming
// writer. For Phase 1, the contents are trace.json + env.json +
// manifest.json + replay.sh — all small.
type Writer struct {
	files map[string][]byte
}

// NewWriter returns an empty bundle builder. Callers add files via
// AddJSON / AddFile, then call WriteTo to emit the tar.gz to a
// filesystem path.
func NewWriter() *Writer {
	return &Writer{files: make(map[string][]byte)}
}

// AddJSON marshals v as indented JSON and stages it under path. Path
// should be relative to the archive root (e.g. "manifest.json",
// "services/db.stdout"). Later files with the same path overwrite
// earlier ones, which matters when a caller wants to rewrite the
// manifest after walking tests.
func (w *Writer) AddJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	w.files[path] = data
	return nil
}

// AddFile stages raw bytes under path. Used for the generated
// `replay.sh` and (later phases) for per-service stdout/stderr that
// the runtime has already captured.
func (w *Writer) AddFile(path string, data []byte) {
	w.files[path] = data
}

// WriteTo flushes the staged files to a tar.gz at dst. The tar layout
// preserves insertion order via a deterministic sort inside tar-write
// so two runs with the same inputs produce byte-identical bundles
// (useful for digest-based reproducibility checks).
func (w *Writer) WriteTo(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}

	buf, err := w.encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	return nil
}

// encode serialises the staged files into a tar.gz byte buffer. Kept
// separate from WriteTo so tests can inspect bundle bytes without
// touching the filesystem.
func (w *Writer) encode() (*bytes.Buffer, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Deterministic iteration order: emit in a stable sequence so two
	// runs with identical inputs produce byte-identical bundles.
	order := sortedPaths(w.files)
	// Use a fixed mtime so digests don't depend on wall-clock — the
	// real timestamp lives in manifest.CreatedAt, which is the source
	// of truth for when the run occurred.
	mtime := time.Unix(0, 0)

	for _, p := range order {
		data := w.files[p]
		hdr := &tar.Header{
			Name:     p,
			Mode:     0o644,
			Size:     int64(len(data)),
			ModTime:  mtime,
			Typeflag: tar.TypeReg,
		}
		if p == "replay.sh" {
			hdr.Mode = 0o755 // executable
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header %s: %w", p, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("tar write %s: %w", p, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return &buf, nil
}

// sortedPaths returns the keys of files in a stable, human-useful
// order: manifest.json first (so `tar -tf` users see the summary up
// front), then env.json, trace.json, replay.sh, then everything else
// alphabetically.
func sortedPaths(files map[string][]byte) []string {
	priority := []string{"manifest.json", "env.json", "trace.json", "replay.sh"}
	out := make([]string, 0, len(files))
	seen := make(map[string]bool)
	for _, p := range priority {
		if _, ok := files[p]; ok {
			out = append(out, p)
			seen[p] = true
		}
	}
	var rest []string
	for p := range files {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	// Sort the tail alphabetically — deterministic, matches the
	// customer's mental model when browsing the archive.
	sortStrings(rest)
	out = append(out, rest...)
	return out
}

// NewRunID returns a short random hex identifier suitable for the
// manifest's run_id field. Not a ULID — we don't need monotonicity
// here since CreatedAt is the ordering field. 128 bits is plenty of
// uniqueness for hand-inspected artifacts.
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to timestamp-based ID if the system rng fails;
		// uniqueness is non-critical for this field.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// DefaultFilename builds the default bundle filename for a run started
// at t with the given seed. The ISO8601 timestamp is compacted
// (colons → dashes) so the filename is legal on every filesystem.
func DefaultFilename(t time.Time, seed uint64) string {
	ts := t.UTC().Format("2006-01-02T15-04-05")
	return fmt.Sprintf("run-%s-%d.fb", ts, seed)
}

// sortStrings is a tiny helper so we don't drag in sort for one call.
func sortStrings(s []string) {
	// Insertion sort — fine for the small file counts we expect per
	// bundle (< 20 top-level entries).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// Compile-time nil-check that the writer satisfies io.Writer-style
// usage elsewhere isn't required; AddFile/AddJSON are the public API.
var _ = io.Copy
