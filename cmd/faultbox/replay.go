package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/faultbox/Faultbox/internal/bundle"
)

// replayCmd implements `faultbox replay <bundle.fb>`. RFC-025
// promised this since v0.9.7; v0.10.0 ships it as the second
// consumer of the bundle format (after `faultbox inspect`).
//
//   faultbox replay run.fb                       # rerun every test
//   faultbox replay run.fb --test test_foo       # rerun one test
//   faultbox replay run.fb --extract-only ./out  # extract spec/ but don't run
//
// Behaviour:
//
//  1. Open the bundle (refuses on unknown schema_version per RFC-025
//     hard gate).
//  2. Apply the version-compat policy: same-major drift warns and
//     proceeds; major drift refuses with a clear error message
//     pointing at the producer version.
//  3. Extract the bundle's spec/ tree to a fresh temp directory.
//  4. Re-invoke testCmd against the extracted root spec, threading
//     the recorded seed so probabilistic faults reproduce.
//
// The bundle's manifest carries `spec_root` so we know which file in
// the spec/ tree was the original LoadFile target — the rest are
// transitive load()s and don't get re-executed independently.
func replayCmd(args []string) int {
	var (
		bundlePath  string
		testFilter  string
		extractOnly string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help":
			printReplayUsage()
			return 0
		case strings.HasPrefix(a, "--test="):
			testFilter = strings.TrimPrefix(a, "--test=")
		case a == "--test" && i+1 < len(args):
			testFilter = args[i+1]
			i++
		case strings.HasPrefix(a, "--extract-only="):
			extractOnly = strings.TrimPrefix(a, "--extract-only=")
		case a == "--extract-only" && i+1 < len(args):
			extractOnly = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
			printReplayUsage()
			return 1
		case bundlePath == "":
			bundlePath = a
		default:
			fmt.Fprintf(os.Stderr, "unexpected argument: %s\n", a)
			printReplayUsage()
			return 1
		}
	}
	if bundlePath == "" {
		fmt.Fprintln(os.Stderr, "faultbox replay: bundle path required")
		printReplayUsage()
		return 1
	}

	r, err := bundle.Open(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if rc := enforceReplayVersionPolicy(os.Stderr, r); rc != 0 {
		return rc
	}

	man := r.Manifest()
	if man.SpecRoot == "" {
		fmt.Fprintln(os.Stderr, "error: bundle manifest has no spec_root — can't determine which spec to replay")
		return 1
	}

	// Extract the spec/ tree to a working directory. Either a user-
	// provided one (--extract-only) or a temp dir we own.
	dst := extractOnly
	cleanup := func() {}
	if dst == "" {
		tmp, err := os.MkdirTemp("", "faultbox-replay-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: mkdtemp: %v\n", err)
			return 1
		}
		dst = tmp
		cleanup = func() { _ = os.RemoveAll(tmp) }
	}

	n, err := extractSpecOnly(r, dst)
	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "error: extract spec/: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Extracted %d spec files to %s\n", n, dst)

	if extractOnly != "" {
		// User asked for extract-only; don't re-run.
		fmt.Fprintln(os.Stderr, "Spec tree extracted; not re-running (--extract-only).")
		fmt.Fprintf(os.Stderr, "To run manually: cd %s && faultbox test %s --seed %d\n",
			dst, man.SpecRoot, man.Seed)
		return 0
	}
	defer cleanup()

	// Synthesise a `faultbox test` invocation against the extracted
	// root spec. Seed comes from the manifest so probabilistic faults
	// reproduce. Bundle output is opt-out by default — the user is
	// reproducing an existing run, they don't need a duplicate
	// bundle of the replay (use --bundle if they do).
	rootInExtract := filepath.Join(dst, man.SpecRoot)
	testArgs := []string{
		rootInExtract,
		"--seed", fmt.Sprintf("%d", man.Seed),
		"--no-bundle",
	}
	if testFilter != "" {
		testArgs = append(testArgs, "--test", testFilter)
	}
	fmt.Fprintf(os.Stderr, "Replaying %s with seed %d\n", man.SpecRoot, man.Seed)
	return testCmd(testArgs)
}

// enforceReplayVersionPolicy implements the RFC-025 version-compat
// matrix specifically for replay: same-major drift warns; major
// drift refuses. Distinct from inspect (which never refuses) so the
// gate stays where the actual re-execution happens.
func enforceReplayVersionPolicy(w *os.File, r *bundle.Reader) int {
	current := faultboxVersion()
	vm := bundle.CheckVersion(r.Manifest().FaultboxVersion, current)
	switch vm.Kind {
	case bundle.VersionSame:
		return 0
	case bundle.VersionMinorPatchDrift, bundle.VersionUnknown:
		fmt.Fprintf(w, "warn: bundle was produced by faultbox %s; current is %s — replaying anyway, behaviour may differ slightly. Install %s for byte-identical replay.\n",
			vm.BundleVer, vm.CurrentVer, vm.BundleVer)
		return 0
	case bundle.VersionMajorDrift:
		fmt.Fprintf(w, "error: bundle was produced by faultbox %s; current is %s — MAJOR version mismatch, replay refuses to avoid silently changed semantics.\n",
			vm.BundleVer, vm.CurrentVer)
		fmt.Fprintf(w, "       Install faultbox %s and try again, or extract the spec manually with: faultbox replay <bundle> --extract-only ./out/\n",
			vm.BundleVer)
		return 2
	}
	return 0
}

// extractSpecOnly writes only the spec/ entries from the bundle into
// dst, preserving relative paths. Bundle has more than spec/ (trace,
// env, replay.sh, services/) but those don't need to materialise on
// disk for the re-run — Faultbox reads the spec, it doesn't need
// the previous run's trace.
func extractSpecOnly(r *bundle.Reader, dst string) (int, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, err
	}
	cleanDst, err := filepath.Abs(dst)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range r.Files() {
		if !strings.HasPrefix(name, "spec/") {
			continue
		}
		rel := strings.TrimPrefix(name, "spec/")
		if rel == "" {
			continue
		}
		target := filepath.Join(cleanDst, rel)
		// Path-traversal guard, mirroring the Reader.Extract behaviour.
		abs, err := filepath.Abs(target)
		if err != nil {
			return n, err
		}
		if !strings.HasPrefix(abs, cleanDst+string(filepath.Separator)) && abs != cleanDst {
			return n, fmt.Errorf("unsafe path %q in bundle", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, err
		}
		data, err := r.File(name)
		if err != nil {
			return n, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func printReplayUsage() {
	const usage = `faultbox replay — re-run a .fb bundle

USAGE
  faultbox replay <bundle.fb>                    # rerun every test
  faultbox replay <bundle.fb> --test <name>      # rerun one test
  faultbox replay <bundle.fb> --extract-only <dir>
      # extract spec/ to dir, don't run; useful for inspecting/editing
      # before re-running manually

EXAMPLES
  faultbox replay run-2026-04-22-42.fb
  faultbox replay run-*.fb --test test_fault_scenario
  faultbox replay run-*.fb --extract-only ./debug/

VERSION COMPATIBILITY
  Same X.Y.Z as the bundle             → silent, replays
  Same major, minor/patch differs      → warns, replays
  Major version differs (e.g. 0.x→1.x) → REFUSES; install bundle's version
`
	fmt.Fprint(os.Stderr, usage)
}
