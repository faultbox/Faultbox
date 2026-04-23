// Package lock implements `faultbox.lock` — the file format that
// pins external dependencies a Faultbox spec resolves so two runs
// (yours Monday, mine Tuesday) reach the exact same bytes.
//
// Today's scope (v0.10.0): container image digests only. Stdlib
// hash and binary checksums are reserved fields in the schema but
// computed in later releases. This keeps the v0.10.0 ship tight
// while leaving the contract stable for downstream tools.
//
// RFC-030.
package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the lock-file schema version emitted by this
// package. Bumps follow the same rules as bundle.SchemaVersion:
// additive field changes don't bump; layout changes do. Tools refuse
// unknown schema versions rather than silently mis-parse.
const SchemaVersion = 1

// Filename is the conventional name for the lock file. Lives next to
// the root spec — `faultbox lock` writes it there; `faultbox test`
// reads it from there.
const Filename = "faultbox.lock"

// Lock is the on-disk shape of faultbox.lock. JSON, sorted keys,
// stable across regenerations so PR diffs stay focused.
type Lock struct {
	SchemaVersion int               `json:"schema_version"`
	LockVersion   string            `json:"lock_version"`              // faultbox version that wrote this file
	GeneratedAt   string            `json:"generated_at"`              // RFC3339; informational
	Images        map[string]string `json:"images,omitempty"`          // tag → "sha256:..."
	Stdlib        *StdlibPin        `json:"stdlib,omitempty"`          // reserved for Phase 2
	Binaries      map[string]string `json:"binaries,omitempty"`        // path → "sha256:..."; reserved for Phase 2
}

// StdlibPin reserves room for a future content-hash of the embedded
// recipes/mocks the producer binary shipped with. Phase 2 of RFC-030.
type StdlibPin struct {
	FaultboxVersion string `json:"faultbox_version"`
	RecipesSHA256   string `json:"recipes_sha256,omitempty"`
}

// New constructs a Lock with the schema/version stamped and the
// generation timestamp set to `now`. Callers fill Images / Binaries
// after instantiation, then call Write.
func New(faultboxVersion string, now time.Time) *Lock {
	return &Lock{
		SchemaVersion: SchemaVersion,
		LockVersion:   faultboxVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Images:        map[string]string{},
	}
}

// Read loads a Lock from path, applying the hard schema-version gate.
// Returns (nil, nil) when the file doesn't exist — callers treat
// "no lock" as "skip verification" since the lock is opt-in.
func Read(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var lk Lock
	if err := json.Unmarshal(data, &lk); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if lk.SchemaVersion == 0 {
		return nil, fmt.Errorf("%s missing schema_version", path)
	}
	if lk.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf(
			"%s schema_version %d is newer than this faultbox supports (%d); upgrade to read",
			path, lk.SchemaVersion, SchemaVersion,
		)
	}
	return &lk, nil
}

// Write serialises the Lock to path with sorted keys and an atomic
// rename, so a concurrent reader never sees a torn write. Pretty-
// prints with two-space indent — the file is meant for humans to PR-
// review.
func (lk *Lock) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lk, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lock: %w", err)
	}
	// Append trailing newline so the file plays nicely with `cat` and
	// editors that complain about missing-EOL.
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Drift describes the differences between an expected Lock (loaded
// from disk) and an observed set of resolved values. Empty means
// "no drift." Callers format these for warnings or fail-strict.
type Drift struct {
	NewImages     []string          // image refs in observed but not in lock
	RemovedImages []string          // refs in lock but not observed
	ChangedImages map[string]Change // refs whose digest moved
}

// Change records the before/after digest pair for a single drifted
// entry. Used inside Drift for both image and (Phase 2) binary diffs.
type Change struct {
	From string
	To   string
}

// Empty reports whether the Drift carries any actual differences.
// Callers use this as a fast "is the lock up to date?" check.
func (d Drift) Empty() bool {
	return len(d.NewImages) == 0 && len(d.RemovedImages) == 0 && len(d.ChangedImages) == 0
}

// CompareImages diffs the lock's image map against an observed map
// (typically `runtime image-pull → digest`). Returns a Drift; empty
// means everything matches.
func (lk *Lock) CompareImages(observed map[string]string) Drift {
	d := Drift{ChangedImages: map[string]Change{}}
	if lk == nil {
		// No lock to compare against; everything is "new" vs nothing.
		// Caller decides whether to treat that as drift.
		for k := range observed {
			d.NewImages = append(d.NewImages, k)
		}
		sort.Strings(d.NewImages)
		return d
	}
	for tag, gotDigest := range observed {
		want, ok := lk.Images[tag]
		switch {
		case !ok:
			d.NewImages = append(d.NewImages, tag)
		case want != gotDigest:
			d.ChangedImages[tag] = Change{From: want, To: gotDigest}
		}
	}
	for tag := range lk.Images {
		if _, ok := observed[tag]; !ok {
			d.RemovedImages = append(d.RemovedImages, tag)
		}
	}
	sort.Strings(d.NewImages)
	sort.Strings(d.RemovedImages)
	return d
}

// Format renders a Drift as a multi-line human-readable warning. One
// line per affected entry; empty when Drift is empty so callers can
// safely concat into log output.
func (d Drift) Format() string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	if len(d.NewImages) > 0 {
		fmt.Fprintf(&b, "  + new images (not in lock): %s\n", strings.Join(d.NewImages, ", "))
	}
	if len(d.RemovedImages) > 0 {
		fmt.Fprintf(&b, "  - removed images (in lock, not in spec): %s\n", strings.Join(d.RemovedImages, ", "))
	}
	for tag, ch := range d.ChangedImages {
		fmt.Fprintf(&b, "  ~ %s: %s → %s\n", tag, shortDigest(ch.From), shortDigest(ch.To))
	}
	return b.String()
}

// shortDigest trims the canonical sha256 prefix so log output isn't
// dominated by 64-char hex strings. Keeps enough to disambiguate.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return "sha256:" + d[:12]
	}
	return "sha256:" + d
}
