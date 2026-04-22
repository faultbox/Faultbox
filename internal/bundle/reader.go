package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Reader is a loaded `.fb` bundle — every file decompressed into
// memory. Bundles are designed to be small (KB–MB), so keeping them
// fully in RAM is simpler than streaming and plays well with the
// random-access needs of `faultbox inspect` (list, dump-one,
// extract-all).
//
// Produced by Open. Callers shouldn't construct Reader directly;
// the exported methods are the contract.
type Reader struct {
	path     string
	files    map[string][]byte
	order    []string // file names in archive order, for deterministic listings
	manifest *Manifest
	env      *Env
}

// Open reads a `.fb` bundle from path into memory and returns a
// Reader. Malformed archives, missing manifest, or unknown
// manifest.schema_version all fail here — we'd rather refuse a
// bundle than silently mis-parse one.
//
// Per the RFC-025 version-compat policy, faultbox_version mismatch is
// NOT a refusal condition here. Callers that care (e.g. replay) inspect
// manifest.FaultboxVersion themselves and decide.
func Open(path string) (*Reader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle %q: %w", path, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip open %q: %w", path, err)
	}
	defer gz.Close()

	r := &Reader{
		path:  path,
		files: make(map[string][]byte),
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read %q: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // skip dirs, symlinks, etc.
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("tar read %q in %q: %w", hdr.Name, path, err)
		}
		r.files[hdr.Name] = body
		r.order = append(r.order, hdr.Name)
	}

	if err := r.loadManifest(); err != nil {
		return nil, err
	}
	if err := r.loadEnv(); err != nil {
		// env.json is required but a parse failure is noisy rather
		// than hard-fatal — the other sections are still useful.
		return nil, fmt.Errorf("env.json parse: %w", err)
	}

	return r, nil
}

// loadManifest parses manifest.json and enforces the hard
// schema_version gate. Unknown schemas refuse here so downstream
// consumers don't have to re-implement the check.
func (r *Reader) loadManifest() error {
	raw, ok := r.files["manifest.json"]
	if !ok {
		return fmt.Errorf("bundle missing manifest.json (not a .fb archive?)")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("manifest.json parse: %w", err)
	}
	if m.SchemaVersion == 0 {
		return fmt.Errorf("manifest.json missing schema_version")
	}
	if m.SchemaVersion > SchemaVersion {
		return fmt.Errorf(
			"bundle schema_version %d is newer than this tool's max (%d); upgrade faultbox to read this bundle",
			m.SchemaVersion, SchemaVersion,
		)
	}
	r.manifest = &m
	return nil
}

// loadEnv parses env.json; it's required in v0.9.7 bundles but
// tolerated-missing here so older hand-crafted bundles (should any
// exist) aren't rejected for a cosmetic field.
func (r *Reader) loadEnv() error {
	raw, ok := r.files["env.json"]
	if !ok {
		r.env = &Env{}
		return nil
	}
	var e Env
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	r.env = &e
	return nil
}

// Path returns the filesystem path this bundle was opened from.
func (r *Reader) Path() string { return r.path }

// Manifest returns the parsed manifest.json. Non-nil on any Reader
// returned by Open (Open refuses bundles with a malformed manifest).
func (r *Reader) Manifest() *Manifest { return r.manifest }

// Env returns the parsed env.json. Returns an empty Env (not nil)
// if env.json was absent, so callers can dereference freely.
func (r *Reader) Env() *Env { return r.env }

// Files returns the archive entry names in archive order (priority
// files first, then alphabetical — matching the writer's ordering).
func (r *Reader) Files() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// File returns the raw bytes of a single entry, or an error if the
// name is not in the bundle. Callers use this for
// `faultbox inspect <bundle> <file>` dumps.
func (r *Reader) File(name string) ([]byte, error) {
	b, ok := r.files[name]
	if !ok {
		return nil, fmt.Errorf("bundle %q has no file %q", r.path, name)
	}
	return b, nil
}

// Extract writes every entry to dst, preserving the archive's
// relative layout. dst is created if missing. Re-extraction over an
// existing directory overwrites matching entries; other files in dst
// are left alone (we don't own the directory).
//
// Returns the number of files written, for caller-side summaries.
func (r *Reader) Extract(dst string) (int, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %q: %w", dst, err)
	}
	cleanDst, err := filepath.Abs(dst)
	if err != nil {
		return 0, fmt.Errorf("resolve %q: %w", dst, err)
	}

	n := 0
	for _, name := range r.order {
		// Hard guard against path traversal: reject any entry whose
		// resolved path escapes dst. Shouldn't happen in practice
		// since our writer only emits safe names, but the check
		// costs nothing and protects users who inspect bundles
		// received from untrusted senders (email, Slack).
		target := filepath.Join(cleanDst, name)
		abs, err := filepath.Abs(target)
		if err != nil {
			return n, fmt.Errorf("resolve %q: %w", name, err)
		}
		if !strings.HasPrefix(abs, cleanDst+string(filepath.Separator)) && abs != cleanDst {
			return n, fmt.Errorf("unsafe path %q in bundle", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, fmt.Errorf("mkdir %q: %w", filepath.Dir(target), err)
		}
		mode := os.FileMode(0o644)
		if name == "replay.sh" {
			mode = 0o755
		}
		if err := os.WriteFile(target, r.files[name], mode); err != nil {
			return n, fmt.Errorf("write %q: %w", target, err)
		}
		n++
	}
	return n, nil
}

// VersionMismatch describes how the bundle's producer version relates
// to the consumer's current version. See RFC-025 comment #59 —
// identical and minor/patch drift are soft; major drift is hard.
type VersionMismatch struct {
	Kind         VersionMismatchKind
	BundleVer    string // from manifest.faultbox_version
	CurrentVer   string // version of the tool doing the check
	BundleMajor  string // "0" for "0.9.7"; convenience for messages
	CurrentMajor string
}

// VersionMismatchKind enumerates the four states the compat policy
// cares about. Callers switch on this and decide whether to silence
// the output, warn, or refuse (replay is the only refuser today).
type VersionMismatchKind int

const (
	// VersionSame — bundle and tool share X.Y.Z.
	VersionSame VersionMismatchKind = iota
	// VersionMinorPatchDrift — same major, minor or patch differs.
	// Callers should warn but proceed.
	VersionMinorPatchDrift
	// VersionMajorDrift — major components differ (0.x ↔ 1.x).
	// `faultbox replay` refuses; `faultbox inspect` still proceeds.
	VersionMajorDrift
	// VersionUnknown — bundle doesn't declare a version, or the
	// version string is not parseable as semver. Treat as minor
	// drift for warning purposes.
	VersionUnknown
)

// CheckVersion implements the soft-gate part of the RFC-025 version
// compatibility policy. It does NOT make the refuse/proceed decision
// — callers do, based on kind. This keeps the policy data-driven and
// test-friendly without scattering "does major match?" checks across
// the codebase.
func CheckVersion(bundleVer, currentVer string) VersionMismatch {
	b := canonicalVersion(bundleVer)
	c := canonicalVersion(currentVer)
	bMajor := majorOf(b)
	cMajor := majorOf(c)

	m := VersionMismatch{
		BundleVer:    bundleVer,
		CurrentVer:   currentVer,
		BundleMajor:  bMajor,
		CurrentMajor: cMajor,
	}
	switch {
	case b == "" || c == "":
		m.Kind = VersionUnknown
	case b == c:
		m.Kind = VersionSame
	case bMajor == "" || cMajor == "":
		// At least one side has a non-semver string like "dev" or
		// "custom". Don't pretend we know whether it's compatible;
		// treat as unknown and let callers decide.
		m.Kind = VersionUnknown
	case bMajor != cMajor:
		m.Kind = VersionMajorDrift
	default:
		m.Kind = VersionMinorPatchDrift
	}
	return m
}

// canonicalVersion strips common adornments ("v" prefix, surrounding
// whitespace) so equality checks don't split hairs over "v0.9.7" vs
// "0.9.7". Kept local to this file because the compat policy is the
// only caller.
func canonicalVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// majorOf returns the numeric major component of a semver-ish string
// like "0.9.7" → "0". Returns "" when the major segment is missing
// or non-numeric — signalling to CheckVersion that the whole string
// should be treated as unknown rather than compared lexically.
func majorOf(v string) string {
	v = canonicalVersion(v)
	if v == "" {
		return ""
	}
	head := v
	if dot := strings.IndexByte(v, '.'); dot >= 0 {
		head = v[:dot]
	}
	if _, err := strconv.Atoi(head); err != nil {
		return ""
	}
	return head
}
