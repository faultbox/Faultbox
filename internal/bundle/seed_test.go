package bundle

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResolveSeedExplicitWinsAndPersists(t *testing.T) {
	dir := t.TempDir()
	seed, source, err := ResolveSeed(dir, 42)
	if err != nil {
		t.Fatalf("ResolveSeed: %v", err)
	}
	if seed != 42 {
		t.Errorf("seed = %d, want 42", seed)
	}
	if source != "cli" {
		t.Errorf("source = %q, want cli", source)
	}
	// Side-effect: subsequent no-flag runs should pick up the same value.
	seed2, source2, _ := ResolveSeed(dir, -1)
	if seed2 != 42 {
		t.Errorf("cached seed = %d, want 42", seed2)
	}
	if source2 != "cached" {
		t.Errorf("source2 = %q, want cached", source2)
	}
}

func TestResolveSeedGeneratesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	seed, source, err := ResolveSeed(dir, -1)
	if err != nil {
		t.Fatalf("ResolveSeed: %v", err)
	}
	if source != "generated" {
		t.Errorf("source = %q, want generated", source)
	}

	// Next call must read the persisted value, not re-generate.
	seed2, source2, _ := ResolveSeed(dir, -1)
	if seed != seed2 {
		t.Errorf("second call seed = %d, want %d (persisted value)", seed2, seed)
	}
	if source2 != "cached" {
		t.Errorf("second source = %q, want cached", source2)
	}

	// Verify the on-disk payload.
	data, err := os.ReadFile(filepath.Join(dir, SeedFile))
	if err != nil {
		t.Fatalf("read seed file: %v", err)
	}
	got, perr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if perr != nil {
		t.Fatalf("parse persisted seed: %v", perr)
	}
	if got != seed {
		t.Errorf("persisted seed = %d, want %d", got, seed)
	}
}

func TestResolveSeedMalformedCacheTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	// Pre-write a malformed cache entry. The resolver should treat it
	// like no-cache and generate+overwrite rather than hard-erroring —
	// a stale cache can't brick the runner.
	cache := filepath.Join(dir, SeedFile)
	_ = os.MkdirAll(filepath.Dir(cache), 0o755)
	_ = os.WriteFile(cache, []byte("not-a-number"), 0o644)

	seed, source, err := ResolveSeed(dir, -1)
	if err != nil {
		t.Fatalf("ResolveSeed: %v", err)
	}
	if source != "generated" {
		t.Errorf("source = %q, want generated (malformed cache)", source)
	}
	if seed == 0 {
		t.Error("generated seed should be non-zero")
	}

	// After recovery the cache should hold the new value.
	if s, ok := readSeedFile(cache); !ok || s != seed {
		t.Errorf("cache not rewritten: ok=%v s=%d want %d", ok, s, seed)
	}
}

func TestResolveSeedExplicitOverwritesCache(t *testing.T) {
	dir := t.TempDir()
	_, _, _ = ResolveSeed(dir, 99) // prime cache with 99

	// Explicit --seed on the next run overwrites the cache.
	_, _, _ = ResolveSeed(dir, 7)

	// A third run with no flag should now see 7, not 99.
	seed, source, _ := ResolveSeed(dir, -1)
	if seed != 7 {
		t.Errorf("cached seed after explicit overwrite = %d, want 7", seed)
	}
	if source != "cached" {
		t.Errorf("source = %q, want cached", source)
	}
}
