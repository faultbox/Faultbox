package sqlmatch

import "testing"

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT 1", "select 1"},
		{"select 1", "select 1"},
		{"  SELECT  1  ", "select 1"},
		{"SELECT 1;", "select 1"},
		{"SELECT 1 ; ;", "select 1"},
		{"SELECT  *\tFROM\nusers", "select * from users"},
		{"SELECT * FROM users WHERE id = ?", "select * from users where id = $?"},
		{"SELECT * FROM users WHERE id = $1", "select * from users where id = $?"},
		{"SELECT * FROM users WHERE id = $42", "select * from users where id = $?"},
		{"SELECT * FROM users WHERE a=? AND b=$2", "select * from users where a = $? and b = $?"},
		{"INSERT INTO t VALUES ('HELLO')", "insert into t values ( 'HELLO' )"},
		{"INSERT INTO t VALUES ('it''s ok')", "insert into t values ( 'it''s ok' )"},
		{"", ""},
		{";;;", ""},
		{"UPDATE users SET role='admin' WHERE id=1", "update users set role = 'admin' where id = 1"},
		{"INSERT INTO t(a,b,c) VALUES(1,2,3)", "insert into t ( a , b , c ) values ( 1 , 2 , 3 )"},
		{"SELECT x WHERE y<=10 AND z!=5", "select x where y <= 10 and z != 5"},
		{"SELECT x WHERE y<>$1", "select x where y <> $?"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := Canonicalize(c.in)
			if got != c.want {
				t.Fatalf("Canonicalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		pattern string
		want    bool
	}{
		// Identity + empty pattern.
		{"empty pattern matches anything", "SELECT 1", "", true},
		{"exact equal", "SELECT 1", "SELECT 1", true},

		// Case folding.
		{"case mismatch query upper pattern lower", "SELECT 1", "select 1", true},
		{"case mismatch query lower pattern upper", "select 1", "SELECT 1", true},

		// Whitespace.
		{"extra spaces in query", "SELECT  *  FROM users", "SELECT * FROM users", true},
		{"tab in query", "SELECT\t*\tFROM users", "SELECT * FROM users", true},
		{"trailing semicolon in query", "SELECT 1;", "SELECT 1", true},
		{"trailing semicolon in pattern", "SELECT 1", "SELECT 1;", true},

		// Placeholder dialects.
		{"question-mark placeholder", "SELECT x WHERE id = ?", "SELECT x WHERE id = ?", true},
		{"$1 matches ?", "SELECT x WHERE id = $1", "SELECT x WHERE id = ?", true},
		{"? matches $1", "SELECT x WHERE id = ?", "SELECT x WHERE id = $1", true},
		{"multi-digit placeholder", "SELECT a, b WHERE x = $10", "SELECT a, b WHERE x = $1", true},

		// Prefix glob.
		{"prefix glob matches INSERT", "INSERT INTO users VALUES (1)", "INSERT*", true},
		{"prefix glob matches lowercased incoming", "insert into users values (1)", "INSERT*", true},
		{"prefix glob does not match UPDATE", "UPDATE users SET x=1", "INSERT*", false},

		// Star inside pattern.
		{"star in middle", "SELECT id, name FROM users WHERE id=1", "SELECT * FROM users*", true},

		// String literals preserved (case-sensitive).
		{"string literal preserved",
			"INSERT INTO t VALUES ('HELLO')",
			"INSERT INTO t VALUES ('HELLO')", true},
		{"string literal case differs — still differs after canonicalization",
			"INSERT INTO t VALUES ('HELLO')",
			"INSERT INTO t VALUES ('hello')", false},

		// Combined mismatches.
		{"all the things at once",
			"  SELECT * FROM USERS WHERE ID=$1 ;",
			"select * from users where id = ?", true},

		// Non-matches.
		{"different statement", "SELECT 1", "UPDATE*", false},
		{"different table prefix", "SELECT * FROM orders", "SELECT * FROM users*", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Match(c.query, c.pattern)
			if got != c.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", c.query, c.pattern, got, c.want)
			}
		})
	}
}
