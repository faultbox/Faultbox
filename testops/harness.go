// Package testops is the Faultbox regression-test harness.
//
// It runs curated Starlark specs through the faultbox CLI and compares
// each run's normalized trace (see internal/star.WriteNormalizedTrace)
// against a committed golden file. A golden mismatch fails the suite.
//
// Run:
//
//	go test ./testops/...              # verify
//	go test ./testops/... -update      # regenerate goldens
//	go test ./testops/... -run mock_demo -v
//
// Phase 0 ships the harness and registry only; goldens are seeded in a
// follow-up once product-level determinism in NormalizeTrace is stable
// (see testops/FINDINGS.md).
package testops

import "time"

// Case is one registered entry in the regression corpus.
//
// Name must be unique and file-system safe; it is used as the golden
// filename (goldens/<name>.norm).
//
// Spec is a path relative to the module root.
//
// Seed pins the deterministic RNG for reproducible traces.
//
// Timeout bounds a single run. Zero means harness default.
//
// LinuxOnly marks specs that require Linux kernel primitives (seccomp
// filters, real-service execution with fault injection). Mock-only
// specs leave this false and run on any host.
//
// Skip, when non-empty, records why the case is temporarily disabled
// (e.g. "requires make demo-build binaries in /tmp"). Skipped cases are
// still listed so the gap is visible; they do not silently disappear.
type Case struct {
	Name      string
	Spec      string
	Seed      int
	Timeout   time.Duration
	LinuxOnly bool
	Skip      string
}

// Cases is the authoritative corpus registry. Adding a case:
//  1. Append an entry here.
//  2. Run: go test ./testops/... -run <name> -update
//  3. Commit goldens/<name>.norm alongside the registry change.
var Cases = []Case{
	{
		Name:    "mock_demo",
		Spec:    "poc/mock-demo/faultbox.star",
		Seed:    1,
		Timeout: 90 * time.Second,
	},
	{
		Name:      "example",
		Spec:      "poc/example/faultbox.star",
		Seed:      1,
		Timeout:   60 * time.Second,
		LinuxOnly: true,
		Skip:      "requires /tmp/mock-db and /tmp/mock-api from make demo-build",
	},
}
