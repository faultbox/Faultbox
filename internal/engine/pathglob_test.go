package engine

import (
	"path/filepath"
	"testing"
)

// M3 (RFC-054): the path-glob retrofit. filepath.Match cannot cross a path
// separator, so `/data/*` matched `/data/foo` but not `/data/a/b` — a rule
// targeting a database that nests its files silently matched nothing.

func TestMatchPathEmptyGlobMatchesEverything(t *testing.T) {
	r := FaultRule{}
	for _, p := range []string{"/tmp/x", "/a/b/c/d", ""} {
		if !r.MatchPath(p) {
			t.Errorf("empty glob rejected %q; it must match everything", p)
		}
	}
}

func TestMatchPathSingleStarDoesNotCrossSegments(t *testing.T) {
	r := FaultRule{PathGlob: "/data/*"}
	if !r.MatchPath("/data/foo") {
		t.Error(`/data/* should match /data/foo`)
	}
	if r.MatchPath("/data/a/b") {
		t.Error(`/data/* must not cross a separator`)
	}
}

// TestMatchPathDoubleStarCrossesSegments is the bug fix itself.
func TestMatchPathDoubleStarCrossesSegments(t *testing.T) {
	cases := []struct {
		glob string
		path string
		want bool
	}{
		{"/data/**", "/data/a/b/c.wal", true},
		{"/data/**", "/data", true},
		{"/data/**/*.wal", "/data/a/b/x.wal", true},
		{"/data/**/*.wal", "/data/x.wal", true},
		{"/data/**/*.wal", "/data/a/x.txt", false},
		{"**/pg_wal/*", "/var/lib/postgresql/data/pg_wal/00000001", true},
		{"/var/lib/postgresql/**/pg_wal/*", "/var/lib/postgresql/data/pg_wal/00000001", true},
	}
	for _, tc := range cases {
		r := FaultRule{PathGlob: tc.glob}
		if got := r.MatchPath(tc.path); got != tc.want {
			t.Errorf("MatchPath(glob=%q, %q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

// TestMatchPathBackCompat is the safety property for existing specs: the
// retrofit must be a widening. Anything filepath.Match accepted must still
// match, or a shipped spec silently stops faulting.
func TestMatchPathBackCompat(t *testing.T) {
	// Globs drawn from the shipped corpus plus the shapes the docs show.
	globs := []string{
		"/tmp/*.wal",
		"/tmp/inventory.wal",
		"/data/*",
		"*.log",
		"/tmp/*",
		"/var/lib/*/data",
		"/tmp/?.wal",
		"/tmp/[abc].wal",
	}
	paths := []string{
		"/tmp/inventory.wal", "/tmp/a.wal", "/tmp/b.wal",
		"/data/foo", "/data/a/b", "app.log",
		"/var/lib/pg/data", "/tmp/nested/deep.wal", "/tmp/",
	}
	for _, g := range globs {
		for _, p := range paths {
			old, err := filepath.Match(g, p)
			if err != nil || !old {
				continue
			}
			if !(FaultRule{PathGlob: g}).MatchPath(p) {
				t.Errorf("REGRESSION: glob %q matched %q before the retrofit but no longer does", g, p)
			}
		}
	}
}

// TestMatchPathFixesTheNestedCase documents what changed, in one assertion.
func TestMatchPathFixesTheNestedCase(t *testing.T) {
	const nested = "/var/lib/postgresql/data/pg_wal/000000010000000000000001"

	// What a user would naturally write before, which matched nothing.
	if (FaultRule{PathGlob: "/var/lib/postgresql/data/*"}).MatchPath(nested) {
		t.Error("single-star glob should still not cross separators")
	}
	// What now works.
	if !(FaultRule{PathGlob: "/var/lib/postgresql/**"}).MatchPath(nested) {
		t.Error("** must reach a nested WAL file — this is the M3 fix")
	}
}

func TestDynamicRuleReportCarriesPathDiagnostics(t *testing.T) {
	s := &Session{ID: "test-session"}
	// Populate dynamicRules directly, as TestDynamicRuleActivityReportsMatchCounts
	// does: SetDynamicFaultRules resolves syscall names through seccomp, which
	// returns -1 on non-Linux hosts and drops the rule.
	s.dynamicRules = map[int32][]*FaultRule{
		1: {{Syscall: "write", Action: ActionDeny, PathGlob: "/data/**/*.wal"}},
	}

	reports := s.DynamicRuleActivity()
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	if reports[0].PathGlob != "/data/**/*.wal" {
		t.Errorf("PathGlob = %q, want the rule's glob", reports[0].PathGlob)
	}
	if reports[0].UnresolvedPaths != 0 {
		t.Errorf("UnresolvedPaths = %d, want 0 with no syscalls seen", reports[0].UnresolvedPaths)
	}

	// Simulate failed /proc path recovery.
	s.unresolvedPaths.Add(3)
	if got := s.DynamicRuleActivity()[0].UnresolvedPaths; got != 3 {
		t.Errorf("UnresolvedPaths = %d, want 3 — a zero-match rule must be able to say why", got)
	}
}
