package pathmatch

import (
	"path/filepath"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Empty pattern is "no filter".
		{"", "/anything", true},
		{"", "", true},

		// Literals.
		{"/tmp/inventory.wal", "/tmp/inventory.wal", true},
		{"/tmp/inventory.wal", "/tmp/other.wal", false},
		{"/tmp/inventory.wal", "/tmp/inventory.wal.bak", false},

		// Single star does NOT cross a separator.
		{"/data/*", "/data/foo", true},
		{"/data/*", "/data/foo.wal", true},
		{"/data/*", "/data/a/b", false},
		{"/data/*", "/data/", true},
		{"*.wal", "x.wal", true},
		{"*.wal", "a/x.wal", false},

		// Double star crosses separators — the whole point of this package.
		{"/data/**", "/data/a/b/c", true},
		{"/data/**", "/data/foo", true},
		{"/data/**", "/data", true},
		{"/data/**", "/other/foo", false},
		{"/data/**/*.wal", "/data/a/b/x.wal", true},
		{"/data/**/*.wal", "/data/x.wal", true},
		{"/data/**/*.wal", "/data/a/x.txt", false},
		{"**/pg_wal/*", "/var/lib/postgresql/data/pg_wal/00000001", true},
		{"**/pg_wal/*", "/var/lib/postgresql/data/base/5/16403", false},
		{"**", "/anything/at/all", true},

		// ? matches one non-separator character.
		{"/tmp/?.wal", "/tmp/a.wal", true},
		{"/tmp/?.wal", "/tmp/ab.wal", false},
		{"/tmp/?.wal", "/tmp//.wal", false},

		// Character classes.
		{"/tmp/[abc].wal", "/tmp/b.wal", true},
		{"/tmp/[abc].wal", "/tmp/d.wal", false},
		{"/tmp/[a-z].wal", "/tmp/q.wal", true},
		{"/tmp/[a-z].wal", "/tmp/Q.wal", false},
		{"/tmp/[!abc].wal", "/tmp/d.wal", true},
		{"/tmp/[!abc].wal", "/tmp/a.wal", false},
		{"/tmp/[^abc].wal", "/tmp/d.wal", true},

		// Escapes.
		{`/tmp/a\*b`, "/tmp/a*b", true},
		{`/tmp/a\*b`, "/tmp/axb", false},

		// Degenerate patterns must not match, not panic.
		{"/tmp/[abc", "/tmp/a", false},
		{`/tmp/a\`, "/tmp/a", false},

		// Realistic WAL targeting, the motivating case.
		{"/var/lib/postgresql/**/pg_wal/*", "/var/lib/postgresql/data/pg_wal/000000010000000000000001", true},
		{"/var/lib/postgresql/data/*", "/var/lib/postgresql/data/pg_wal/000000010000000000000001", false},
	}

	for _, tc := range tests {
		t.Run(tc.pattern+" vs "+tc.path, func(t *testing.T) {
			if got := Match(tc.pattern, tc.path); got != tc.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// TestBackCompatWithFilepathMatch is the load-bearing safety property: this
// retrofit must be a *widening*. Every (pattern, path) pair that
// filepath.Match accepted must still match, or an existing spec silently
// stops faulting.
//
// The converse does not hold, and that is the fix: patterns that used to match
// nothing now match.
func TestBackCompatWithFilepathMatch(t *testing.T) {
	patterns := []string{
		"/tmp/*.wal", "/data/*", "*.log", "/tmp/inventory.wal",
		"/tmp/?.wal", "/tmp/[abc].wal", "/var/lib/*/data", "*",
		"/tmp/*", "/a/b/c", "[a-z]*.db",
	}
	paths := []string{
		"/tmp/inventory.wal", "/tmp/a.wal", "/data/foo", "/data/a/b",
		"app.log", "/var/lib/pg/data", "/a/b/c", "x.db", "/tmp/",
		"/tmp/nested/deep.wal", "", "abc",
	}

	for _, p := range patterns {
		for _, s := range paths {
			old, err := filepath.Match(p, s)
			if err != nil {
				continue // malformed for stdlib; our dialect may differ deliberately
			}
			if !old {
				continue // only the "used to match" direction is a compatibility constraint
			}
			if !Match(p, s) {
				t.Errorf("REGRESSION: filepath.Match(%q, %q) was true but pathmatch says false", p, s)
			}
		}
	}
}

// TestDoubleStarIsStrictlyMoreThanSingle documents the actual fix.
func TestDoubleStarIsStrictlyMoreThanSingle(t *testing.T) {
	const nested = "/data/a/b/c.wal"
	if Match("/data/*", nested) {
		t.Error("/data/* should not cross separators")
	}
	if !Match("/data/**", nested) {
		t.Error("/data/** must cross separators — this is the bug being fixed")
	}
	// And what filepath.Match did before: nothing.
	if ok, _ := filepath.Match("/data/**", nested); ok {
		t.Log("note: filepath.Match happens to accept this input too")
	}
}

func TestMatchAny(t *testing.T) {
	if !MatchAny(nil, "/anything") {
		t.Error("empty pattern list must match everything")
	}
	pats := []string{"/data/**", "/tmp/*.wal"}
	if !MatchAny(pats, "/data/a/b") {
		t.Error("first pattern should match")
	}
	if !MatchAny(pats, "/tmp/x.wal") {
		t.Error("second pattern should match")
	}
	if MatchAny(pats, "/etc/passwd") {
		t.Error("neither pattern should match")
	}
}

func TestHasWildcard(t *testing.T) {
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{"/tmp/x.wal", false},
		{"/tmp/*.wal", true},
		{"/tmp/?.wal", true},
		{"/tmp/[ab].wal", true},
		{"", false},
	} {
		if got := HasWildcard(tc.p); got != tc.want {
			t.Errorf("HasWildcard(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// Patterns are evaluated per syscall on the fault datapath, so this must not
// be pathological.
func BenchmarkMatchLiteral(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Match("/var/lib/postgresql/data/pg_wal/0001", "/var/lib/postgresql/data/pg_wal/0001")
	}
}

func BenchmarkMatchSingleStar(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Match("/var/lib/postgresql/data/pg_wal/*", "/var/lib/postgresql/data/pg_wal/0001")
	}
}

func BenchmarkMatchDoubleStar(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Match("/var/**/pg_wal/*", "/var/lib/postgresql/data/pg_wal/0001")
	}
}
