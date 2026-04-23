package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/faultbox/Faultbox/internal/container"
	"github.com/faultbox/Faultbox/internal/lock"
	"github.com/faultbox/Faultbox/internal/star"
)

// newQuietLogger returns a slog logger that drops everything below
// WARN to /dev/null. The lock subcommand and (later) test integration
// don't need the runtime's INFO-level chatter; they just need errors
// to surface.
func newQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// lockCmd implements `faultbox lock` per RFC-030. Three modes:
//
//   faultbox lock [spec.star]            # generate, write faultbox.lock
//   faultbox lock --check [spec.star]    # exit 0 if matches, 2 if drifted
//   faultbox lock --update [spec.star]   # alias for the default; explicit-update intent
//
// All three resolve the same way: load the spec to enumerate every
// image= reference, then ask Docker for the canonical digest of each.
//
// Spec path is optional; defaults to faultbox.star in cwd. The lock
// file lives next to the spec.
func lockCmd(args []string) int {
	var (
		specPath string
		mode     = "write" // "write" | "check"
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help":
			printLockUsage()
			return 0
		case a == "--check":
			mode = "check"
		case a == "--update":
			mode = "write" // same as default; explicit for scripting clarity
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
			printLockUsage()
			return 1
		case specPath == "":
			specPath = a
		default:
			fmt.Fprintf(os.Stderr, "unexpected argument: %s\n", a)
			printLockUsage()
			return 1
		}
	}
	if specPath == "" {
		specPath = "faultbox.star"
	}
	if _, err := os.Stat(specPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", specPath, err)
		return 1
	}

	// Load the spec just for service enumeration. We don't run tests
	// here — Starlark evaluation happens in LoadFile and registers
	// services via the runtime.
	rt := star.New(newQuietLogger())
	if err := rt.LoadFile(specPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: load spec: %v\n", err)
		return 1
	}

	images := collectImages(rt)
	if len(images) == 0 {
		fmt.Fprintln(os.Stderr, "no container images referenced in spec; lock would be empty")
		// Still write/check so the file's lifecycle stays predictable.
	}

	resolved, err := resolveImageDigests(context.Background(), images)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve digests: %v\n", err)
		return 1
	}

	lockPath := filepath.Join(filepath.Dir(specPath), lock.Filename)

	switch mode {
	case "check":
		return runLockCheck(lockPath, resolved)
	case "write":
		return runLockWrite(lockPath, resolved)
	}
	return 0
}

// runLockWrite generates a fresh lock from the resolved digests and
// writes it. Always overwrites — this is the lifecycle the user
// asked for by running the command.
func runLockWrite(path string, resolved map[string]string) int {
	lk := lock.New(faultboxVersion(), time.Now())
	for tag, digest := range resolved {
		lk.Images[tag] = digest
	}
	if err := lk.Write(path); err != nil {
		fmt.Fprintf(os.Stderr, "error: write lock: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Wrote %s\n", path)
	for _, tag := range sortedKeys(resolved) {
		fmt.Fprintf(os.Stderr, "  %s → %s\n", tag, shortenDigest(resolved[tag]))
	}
	return 0
}

// runLockCheck loads the existing lock and diffs it against current
// resolved digests. Exit 0 when clean; 2 when drifted; 1 on infra
// errors. CI scripts use the exit code.
func runLockCheck(path string, resolved map[string]string) int {
	lk, err := lock.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read lock: %v\n", err)
		return 1
	}
	if lk == nil {
		fmt.Fprintf(os.Stderr, "error: %s does not exist; run \"faultbox lock\" to create it\n", path)
		return 1
	}
	drift := lk.CompareImages(resolved)
	if drift.Empty() {
		fmt.Fprintln(os.Stderr, "Lock is up to date.")
		return 0
	}
	fmt.Fprintf(os.Stderr, "Lock drift detected:\n%s", drift.Format())
	fmt.Fprintf(os.Stderr, "Run \"faultbox lock --update\" to refresh.\n")
	return 2
}

// collectImages walks every registered service and returns the set
// of image references that need pinning. Mock and binary services
// have no Image so they're naturally excluded.
func collectImages(rt *star.Runtime) []string {
	seen := make(map[string]bool)
	for _, svc := range rt.Services() {
		if svc.Image == "" {
			continue
		}
		seen[svc.Image] = true
	}
	out := make([]string, 0, len(seen))
	for img := range seen {
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}

// resolveImageDigests pulls (if needed) and reads the digest for each
// reference. Uses the standard container.Client; respects the user's
// docker config (registry creds, tls). Errors fast on the first
// failure — a partially-resolved lock is worse than no lock.
func resolveImageDigests(ctx context.Context, refs []string) (map[string]string, error) {
	if len(refs) == 0 {
		return map[string]string{}, nil
	}
	cli, err := container.NewClient(ctx, newQuietLogger())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		fmt.Fprintf(os.Stderr, "Resolving %s ...\n", ref)
		digest, err := cli.ImageDigest(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		out[ref] = digest
	}
	return out, nil
}

// sortedKeys returns the keys of m in lexical order. Used for
// deterministic CLI output.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shortenDigest mirrors the lock package's helper but stays here
// because main can't import the lock package's unexported helper.
// Trims the long hex so user output is scannable.
func shortenDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return "sha256:" + d[:12] + "..."
	}
	return "sha256:" + d
}

// preflightLockCheck reads faultbox.lock (if present) and notes its
// presence in stderr so users in CI know it's being respected.
// Returns nonzero exit code only when:
//   - The file is malformed (parse error, future schema_version)
//   - FAULTBOX_LOCK_STRICT=1 AND the file is required (future
//     enforcement; today this is a soft check)
//
// Image-digest comparison happens later in the run lifecycle after
// services have resolved their images; this preflight just makes
// the file's presence visible up front. Phase 2 of RFC-030 wires the
// strict comparison into the pull path.
func preflightLockCheck(specDir string) int {
	path := filepath.Join(specDir, lock.Filename)
	lk, err := lock.Read(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: lock file %s: %v\n", path, err)
		return 1
	}
	if lk == nil {
		// No lock file — silent. This is the "users haven't opted in
		// yet" case; we don't want to nag every test run.
		if os.Getenv("FAULTBOX_LOCK_STRICT") == "1" {
			fmt.Fprintf(os.Stderr, "error: FAULTBOX_LOCK_STRICT=1 but %s does not exist; run \"faultbox lock\" first\n", path)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "Lock: %s (faultbox %s, %d image%s)\n",
		path, lk.LockVersion, len(lk.Images), pluralS(len(lk.Images)))
	return 0
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func printLockUsage() {
	const usage = `faultbox lock — pin container image digests for reproducible runs

USAGE
  faultbox lock [spec.star]              # generate / overwrite faultbox.lock
  faultbox lock --check [spec.star]      # exit 0 if lock matches; 2 if drift
  faultbox lock --update [spec.star]     # explicit alias for the default

The lock file lives next to the spec. spec.star defaults to
faultbox.star in cwd.

CI USAGE
  faultbox lock --check                  # fails build if lock is stale
  FAULTBOX_LOCK_STRICT=1 faultbox test   # fail tests on drift, not just warn

EXAMPLES
  faultbox lock                          # write faultbox.lock from cwd's faultbox.star
  faultbox lock infra/specs/auth.star    # lock for a non-default spec path
  faultbox lock --check                  # exit nonzero if drifted
`
	fmt.Fprint(os.Stderr, usage)
}
