package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestPostgresProxy_CheckRules_SQLCanonicalization is the Postgres twin of
// TestMySQLProxy_CheckRules_SQLCanonicalization: same canonicalizer hits
// the shared sqlmatch package, so the observable contract must be the same
// for both proxies.
func TestPostgresProxy_CheckRules_SQLCanonicalization(t *testing.T) {
	cases := []struct {
		name        string
		rulePattern string
		query       string
		wantHandled bool
	}{
		{
			name:        "tight driver output matches spaced rule pattern",
			rulePattern: "SELECT * FROM users WHERE id = $1",
			query:       "select * from users where id=$1;",
			wantHandled: true,
		},
		{
			name:        "question-mark rule matches dollar placeholder query",
			rulePattern: "UPDATE users SET role = ? WHERE id = ?",
			query:       "UPDATE users SET role=$1 WHERE id=$2",
			wantHandled: true,
		},
		{
			name:        "INSERT glob fires",
			rulePattern: "INSERT*",
			query:       "insert into users(id, email) values ($1, $2)",
			wantHandled: true,
		},
		{
			name:        "SELECT does not match UPDATE glob",
			rulePattern: "UPDATE*",
			query:       "SELECT count(*) FROM users",
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPostgresProxy(nil, "test-svc")
			p.AddRule(Rule{
				Query:  tc.rulePattern,
				Action: ActionError,
				Error:  "injected",
			})

			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			go func() {
				client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = io.Copy(io.Discard, client)
			}()

			handled := p.checkRules(server, tc.query)
			if handled != tc.wantHandled {
				t.Fatalf("checkRules(%q) with rule %q: got handled=%v, want %v",
					tc.query, tc.rulePattern, handled, tc.wantHandled)
			}
		})
	}
}
