package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestMySQLProxy_CheckRules_SQLCanonicalization verifies that the MySQL
// proxy's rule matcher canonicalizes SQL before comparing — so a rule
// keyed on "SELECT * FROM users WHERE id = ?" matches an incoming
// "select * from users where id=$1;" that a driver might actually send.
//
// Without canonicalization this case would miss: filepath.Match is
// case-sensitive on the non-wildcard portion and strings.EqualFold only
// helps on full-string equality, not on placeholder or whitespace drift.
func TestMySQLProxy_CheckRules_SQLCanonicalization(t *testing.T) {
	cases := []struct {
		name        string
		rulePattern string
		query       string
		wantHandled bool
	}{
		{
			name:        "tight driver output matches spaced rule pattern",
			rulePattern: "SELECT * FROM users WHERE id = ?",
			query:       "select * from users where id=$1;",
			wantHandled: true,
		},
		{
			name:        "whitespace + case + trailing semicolon",
			rulePattern: "UPDATE users SET role = ? WHERE id = ?",
			query:       "  UPDATE  users  SET role=$1 WHERE id=$2 ;",
			wantHandled: true,
		},
		{
			name:        "prefix glob on INSERT fires on lowercase driver output",
			rulePattern: "INSERT*",
			query:       "insert into users values (1, 'a')",
			wantHandled: true,
		},
		{
			name:        "non-matching statement is not handled",
			rulePattern: "UPDATE*",
			query:       "SELECT 1",
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newMySQLProxy(nil, "test-svc")
			p.AddRule(Rule{
				Query:  tc.rulePattern,
				Action: ActionError,
				Error:  "injected",
			})

			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			// Continuously drain whatever the proxy writes so checkRules
			// never blocks on a pipe Write.
			go func() {
				client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = io.Copy(io.Discard, client)
			}()

			handled := p.checkRules(server, 0, tc.query)
			if handled != tc.wantHandled {
				t.Fatalf("checkRules(%q) with rule %q: got handled=%v, want %v",
					tc.query, tc.rulePattern, handled, tc.wantHandled)
			}
		})
	}
}

// TestMySQLProxy_CheckRules_Probability verifies that Prob on a matching
// rule gates the action — over 2000 trials at Prob=0.3, the observed
// hit rate should fall within a loose ±10% band of the target.
//
// The test uses a matching query every time, so any miss is purely the
// probability gate (not a canonicalization miss). A failure here would
// indicate Prob is dropped somewhere in the plumbing — today it arrives
// from Starlark via the ProxyFaultDef.Probability field.
func TestMySQLProxy_CheckRules_Probability(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	p.AddRule(Rule{
		Query:  "SELECT *",
		Action: ActionError,
		Error:  "maybe",
		Prob:   0.3,
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, client)
	}()

	const trials = 2000
	hits := 0
	for i := 0; i < trials; i++ {
		if p.checkRules(server, 0, "SELECT 1") {
			hits++
		}
	}

	rate := float64(hits) / float64(trials)
	if rate < 0.20 || rate > 0.40 {
		t.Fatalf("Prob=0.3 produced hit rate %.3f over %d trials — expected 0.20..0.40",
			rate, trials)
	}
}

// TestMySQLProxy_CheckRules_EmptyPatternMatchesAll verifies that a rule
// with an empty Query pattern fires on every query — preserves prior
// "no-query-filter = match-all" behavior after the canonicalizer refactor.
func TestMySQLProxy_CheckRules_EmptyPatternMatchesAll(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	p.AddRule(Rule{
		Query:  "", // match all
		Action: ActionError,
		Error:  "all queries fail",
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = io.Copy(io.Discard, client)
	}()

	if !p.checkRules(server, 0, "SELECT anything at all") {
		t.Fatal("expected match-all rule to fire")
	}
}
