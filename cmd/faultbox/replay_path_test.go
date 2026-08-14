package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/star"
)

// TestExtractedRootNameHandlesSubdirectorySpecs is F-6.
//
// spec_root records the path as typed on the command line; the bundle
// stores specs relative to the root spec's own directory, so the root is
// always at `spec/<basename>`. Joining the extraction dir with spec_root
// therefore looked for a directory that does not exist in the archive.
//
// A spec at the repo root makes the two spellings identical, which is
// why every spec in this repo replayed fine and this went unseen.
func TestExtractedRootNameHandlesSubdirectorySpecs(t *testing.T) {
	tests := []struct {
		name     string
		specRoot string
		want     string
	}{
		{"repo-root spec is unchanged", "faultbox.star", "faultbox.star"},
		{"subdirectory spec drops its directory", "faultbox/spec.star", "spec.star"},
		{"deeper nesting drops all of it", "test/integration/fb.star", "fb.star"},
		{"leading ./ is not mistaken for a directory", "./faultbox.star", "faultbox.star"},
		{"absolute path drops to the basename", "/repo/faultbox/spec.star", "spec.star"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractedRootName(tt.specRoot); got != tt.want {
				t.Errorf("extractedRootName(%q) = %q, want %q", tt.specRoot, got, tt.want)
			}
		})
	}
}

// TestExtractedRootJoinsInsideTheExtractionDir guards the property that
// actually matters: the resolved path stays inside dst and names a file
// the extractor wrote.
func TestExtractedRootJoinsInsideTheExtractionDir(t *testing.T) {
	dst := t.TempDir()
	got := filepath.Join(dst, extractedRootName("faultbox/spec.star"))
	want := filepath.Join(dst, "spec.star")

	if got != want {
		t.Errorf("root resolved to %q, want %q", got, want)
	}
	if strings.Contains(got, "faultbox"+string(filepath.Separator)+"spec.star") {
		t.Error("resolved path still carries the original directory component")
	}
}

// TestReplayHintIsAcceptedByReplay asserts the printed suggestion is a
// command `replay` will actually run.
//
// The hint used to read `faultbox replay <bundle.fb> --test X --seed N`,
// but replay's parser accepts only --test: it takes the seed from the
// manifest, which is the point of replaying a bundle. So every
// copy-pasted hint failed on an unknown flag before it could reach the
// path bug above.
func TestReplayHintIsAcceptedByReplay(t *testing.T) {
	var buf bytes.Buffer
	printReplayHints(&buf, &star.SuiteResult{
		Tests: []star.TestResult{
			{Name: "test_order_flow", Result: "fail", Seed: 42},
		},
	})

	hint := strings.TrimSpace(buf.String())
	if hint == "" {
		t.Fatal("no replay hint was printed for a failed test")
	}
	if strings.Contains(hint, "--seed") {
		t.Errorf("hint carries --seed, which replay rejects: %q", hint)
	}
	if !strings.Contains(hint, "--test test_order_flow") {
		t.Errorf("hint does not name the failed test: %q", hint)
	}

	// Every flag in the hint must be one replay parses. Keeping this as a
	// loop rather than a fixed string means a future flag added to the
	// hint has to be added to replay too.
	accepted := map[string]bool{"--test": true}
	for _, field := range strings.Fields(hint) {
		if !strings.HasPrefix(field, "--") {
			continue
		}
		flag := field
		if i := strings.Index(field, "="); i >= 0 {
			flag = field[:i]
		}
		if !accepted[flag] {
			t.Errorf("hint uses %q, which replay does not accept", flag)
		}
	}
}
