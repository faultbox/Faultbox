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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HashBinary returns the canonical "sha256:<hex>" digest of the file
// at path. Used by faultbox lock to pin volume-mounted binaries
// (#77). The whole-file hash is the supply-chain-correct answer —
// a 50 MB Go binary takes ~100ms which is acceptable for lock-time
// work.
func HashBinary(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

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
//
// observed and locked are populated by CompareImages and used by
// Format to render an actionable per-row "locked vs current" view
// (#82). They mirror the data already in NewImages/RemovedImages/
// ChangedImages — the maps are kept around so Format doesn't have
// to be re-threaded through callers' Lock + observed scope.
type Drift struct {
	NewImages     []string          // image refs in observed but not in lock
	RemovedImages []string          // refs in lock but not observed
	ChangedImages map[string]Change // refs whose digest moved

	NewBinaries     []string          // binary paths in observed but not in lock
	RemovedBinaries []string          // paths in lock but not observed
	ChangedBinaries map[string]Change // paths whose digest moved

	observed         map[string]string // tag → observed digest
	locked           map[string]string // tag → locked digest
	observedBinaries map[string]string // path → observed digest
	lockedBinaries   map[string]string // path → locked digest
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
	return len(d.NewImages) == 0 && len(d.RemovedImages) == 0 && len(d.ChangedImages) == 0 &&
		len(d.NewBinaries) == 0 && len(d.RemovedBinaries) == 0 && len(d.ChangedBinaries) == 0
}

// CompareBinaries diffs the lock's binaries map against an observed
// map (typically `service binary path → sha256(file)`). Mirrors
// CompareImages — kept as a separate method so callers can pin
// images and binaries independently. Returns a Drift carrying only
// the binary fields populated; merge via MergeBinaries when both
// are needed in one report. (#77)
func (lk *Lock) CompareBinaries(observed map[string]string) Drift {
	d := Drift{
		ChangedBinaries:  map[string]Change{},
		observedBinaries: observed,
		lockedBinaries:   map[string]string{},
	}
	if lk == nil {
		for k := range observed {
			d.NewBinaries = append(d.NewBinaries, k)
		}
		sort.Strings(d.NewBinaries)
		return d
	}
	for path, dg := range lk.Binaries {
		d.lockedBinaries[path] = dg
	}
	for path, gotDigest := range observed {
		want, ok := lk.Binaries[path]
		switch {
		case !ok:
			d.NewBinaries = append(d.NewBinaries, path)
		case want != gotDigest:
			d.ChangedBinaries[path] = Change{From: want, To: gotDigest}
		}
	}
	for path := range lk.Binaries {
		if _, ok := observed[path]; !ok {
			d.RemovedBinaries = append(d.RemovedBinaries, path)
		}
	}
	sort.Strings(d.NewBinaries)
	sort.Strings(d.RemovedBinaries)
	return d
}

// MergeBinaries copies the binary-side fields from b into d in
// place. Lets callers compose CompareImages + CompareBinaries
// results into a single Drift for unified Format() output.
func (d *Drift) MergeBinaries(b Drift) {
	d.NewBinaries = append(d.NewBinaries, b.NewBinaries...)
	d.RemovedBinaries = append(d.RemovedBinaries, b.RemovedBinaries...)
	if d.ChangedBinaries == nil {
		d.ChangedBinaries = map[string]Change{}
	}
	for k, v := range b.ChangedBinaries {
		d.ChangedBinaries[k] = v
	}
	if d.observedBinaries == nil {
		d.observedBinaries = map[string]string{}
	}
	for k, v := range b.observedBinaries {
		d.observedBinaries[k] = v
	}
	if d.lockedBinaries == nil {
		d.lockedBinaries = map[string]string{}
	}
	for k, v := range b.lockedBinaries {
		d.lockedBinaries[k] = v
	}
}

// CompareImages diffs the lock's image map against an observed map
// (typically `runtime image-pull → digest`). Returns a Drift; empty
// means everything matches.
func (lk *Lock) CompareImages(observed map[string]string) Drift {
	d := Drift{
		ChangedImages: map[string]Change{},
		observed:      observed,
		locked:        map[string]string{},
	}
	if lk == nil {
		// No lock to compare against; everything is "new" vs nothing.
		// Caller decides whether to treat that as drift.
		for k := range observed {
			d.NewImages = append(d.NewImages, k)
		}
		sort.Strings(d.NewImages)
		return d
	}
	for tag, dg := range lk.Images {
		d.locked[tag] = dg
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

// Format renders a Drift as a multi-line human-readable warning. The
// output is a per-row "locked vs current" table that names every
// drifted entry — actionable from CI logs without needing to re-run
// locally to figure out what changed (#82).
//
//	drift detected (3 entries):
//	  mysql:8.0.32   locked sha256:abc123…   current sha256:def456…
//	  redis:7        locked sha256:9876…     current <not found locally>
//	  postgres:16    locked <not in lock>    current sha256:1111…
//
// Empty when Drift is empty so callers can safely concat into log
// output.
func (d Drift) Format() string {
	if d.Empty() {
		return ""
	}

	type row struct{ kind, tag, locked, current string }
	rows := make([]row, 0,
		len(d.NewImages)+len(d.RemovedImages)+len(d.ChangedImages)+
			len(d.NewBinaries)+len(d.RemovedBinaries)+len(d.ChangedBinaries))

	for _, tag := range d.NewImages {
		rows = append(rows, row{
			kind:    "image",
			tag:     tag,
			locked:  "<not in lock>",
			current: digestForDisplay(d.observed[tag]),
		})
	}
	for _, tag := range d.RemovedImages {
		rows = append(rows, row{
			kind:    "image",
			tag:     tag,
			locked:  digestForDisplay(d.locked[tag]),
			current: "<not found locally>",
		})
	}
	// Stable ordering for changed entries — map iteration is otherwise
	// non-deterministic and we want CI logs to diff cleanly.
	changedTags := make([]string, 0, len(d.ChangedImages))
	for tag := range d.ChangedImages {
		changedTags = append(changedTags, tag)
	}
	sort.Strings(changedTags)
	for _, tag := range changedTags {
		ch := d.ChangedImages[tag]
		rows = append(rows, row{
			kind:    "image",
			tag:     tag,
			locked:  digestForDisplay(ch.From),
			current: digestForDisplay(ch.To),
		})
	}

	for _, p := range d.NewBinaries {
		rows = append(rows, row{
			kind:    "binary",
			tag:     p,
			locked:  "<not in lock>",
			current: digestForDisplay(d.observedBinaries[p]),
		})
	}
	for _, p := range d.RemovedBinaries {
		rows = append(rows, row{
			kind:    "binary",
			tag:     p,
			locked:  digestForDisplay(d.lockedBinaries[p]),
			current: "<not found on disk>",
		})
	}
	changedBins := make([]string, 0, len(d.ChangedBinaries))
	for p := range d.ChangedBinaries {
		changedBins = append(changedBins, p)
	}
	sort.Strings(changedBins)
	for _, p := range changedBins {
		ch := d.ChangedBinaries[p]
		rows = append(rows, row{
			kind:    "binary",
			tag:     p,
			locked:  digestForDisplay(ch.From),
			current: digestForDisplay(ch.To),
		})
	}

	// Column widths so the locked / current columns line up visually.
	kindW, tagW, lockedW := 0, 0, 0
	for _, r := range rows {
		if len(r.kind) > kindW {
			kindW = len(r.kind)
		}
		if len(r.tag) > tagW {
			tagW = len(r.tag)
		}
		if len(r.locked) > lockedW {
			lockedW = len(r.locked)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "drift detected (%d entries):\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %-*s   locked %-*s   current %s\n",
			kindW, r.kind, tagW, r.tag, lockedW, r.locked, r.current)
	}
	return b.String()
}

// digestForDisplay shortens sha256 digests for tabular output and
// passes through "<…>" placeholder strings unchanged.
func digestForDisplay(d string) string {
	if d == "" {
		return "<unknown>"
	}
	if strings.HasPrefix(d, "<") {
		return d
	}
	return shortDigest(d)
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
