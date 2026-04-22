package bundle

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SeedFile is the location (relative to the spec's directory) where
// `faultbox test` persists the seed used for the most recent run so
// subsequent invocations without an explicit --seed can reuse it.
// Pattern: "first rerun reproduces the last failure." v0.9.8 per
// customer ask C3.
const SeedFile = ".faultbox/last-seed"

// ResolveSeed decides what seed to use for a run.
//
//   - If explicit >= 0, it's whatever the user passed on the CLI. Always wins,
//     and we persist it so subsequent runs with no flag continue to use it
//     until the user picks a new one.
//   - Otherwise we look in .faultbox/last-seed under specDir. If present,
//     we reuse it. If not, we generate a fresh random seed and persist it.
//
// Returns the seed plus a "source" string ("cli", "cached", or "generated")
// that callers surface to users so they see what happened. specDir is the
// directory containing the root .star file (rt.baseDir in the runtime).
func ResolveSeed(specDir string, explicit int64) (seed uint64, source string, err error) {
	path := filepath.Join(specDir, SeedFile)

	if explicit >= 0 {
		s := uint64(explicit)
		if werr := writeSeedFile(path, s); werr != nil {
			// Non-fatal — the seed is still used for this run. But
			// surface via the returned source so callers can log it.
			return s, "cli (warn: cache write failed: " + werr.Error() + ")", nil
		}
		return s, "cli", nil
	}

	if s, ok := readSeedFile(path); ok {
		return s, "cached", nil
	}

	s, err := randomSeed()
	if err != nil {
		return 0, "", fmt.Errorf("generate seed: %w", err)
	}
	if werr := writeSeedFile(path, s); werr != nil {
		// Still return the seed; caller can proceed.
		return s, "generated (warn: cache write failed: " + werr.Error() + ")", nil
	}
	return s, "generated", nil
}

// readSeedFile returns (seed, true) if the file exists and is parseable
// as a decimal uint64; otherwise (0, false). Unreadable / malformed
// files are treated the same as missing so a stale cache never blocks a
// run — the next write refreshes it.
func readSeedFile(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s, perr := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if perr != nil {
		return 0, false
	}
	return s, true
}

// writeSeedFile persists seed to path atomically (write temp + rename)
// so a concurrent reader never sees a torn write. Creates the parent
// directory if missing so the first-run case just works.
func writeSeedFile(path string, seed uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(seed, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// randomSeed produces a cryptographically-random 63-bit seed (one bit
// shy of uint64 so callers that cast to int64 can't trip the sign bit).
// Used when no explicit --seed was passed and no cached seed exists.
func randomSeed() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b[:]) &^ (1 << 63), nil
}
