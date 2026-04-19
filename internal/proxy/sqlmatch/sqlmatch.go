// Package sqlmatch provides normalization + matching helpers for SQL
// query patterns used by the MySQL and Postgres proxies' fault rules.
//
// Without normalization, a rule keyed on
//
//	"SELECT * FROM users WHERE id = ?"
//
// misses an incoming query of
//
//	"select * from users where id=$1;"
//
// because of case, whitespace, placeholder dialect, and trailing `;`.
// Canonicalize folds those differences out so rule authors don't have to
// guess how each driver rewrites their SQL.
package sqlmatch

import (
	"path/filepath"
	"strings"
)

// Canonicalize returns a normalized form of the SQL for matching. The
// transformation is lossy — it's meant for equality / glob matching, not
// round-trip re-execution.
//
// Rules applied (outside single-quoted string literals):
//  1. Trim surrounding whitespace and drop any trailing ';'.
//  2. Lowercase ASCII letters.
//  3. Replace the placeholder forms '?' and '$N' (numeric $1, $2, ...) with
//     a canonical '$?' marker — so MySQL-style and Postgres-style prepared
//     statements normalize to the same shape.
//  4. Insert whitespace around the comparison and list operators
//     '=', '<', '>', '!=', '<>', '<=', '>=', ',', '(', ')' so that tight
//     driver output ("id=$1") matches user-authored spaced patterns
//     ("id = ?").
//  5. Collapse any run of whitespace to a single space.
//
// Contents of single-quoted string literals are preserved verbatim. Glob
// wildcards ('*') are not touched by any of the rules above, so Canonicalize
// is safe to apply to both the incoming query and the rule pattern.
func Canonicalize(sql string) string {
	// Strip surrounding whitespace and trailing ';'.
	sql = strings.TrimSpace(sql)
	for strings.HasSuffix(sql, ";") {
		sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	}
	if sql == "" {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(sql) + 16)
	inString := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]

		if c == '\'' {
			inString = !inString
			sb.WriteByte(c)
			continue
		}
		if inString {
			sb.WriteByte(c)
			continue
		}

		// Two-char comparison operators. Check before single-char so '<=' is
		// not eaten as a standalone '<'.
		if i+1 < len(sql) {
			two := sql[i : i+2]
			if two == "!=" || two == "<>" || two == "<=" || two == ">=" {
				sb.WriteByte(' ')
				sb.WriteString(two)
				sb.WriteByte(' ')
				i++
				continue
			}
		}

		// Single-char operators / list punctuation.
		if c == '=' || c == '<' || c == '>' || c == ',' || c == '(' || c == ')' {
			sb.WriteByte(' ')
			sb.WriteByte(c)
			sb.WriteByte(' ')
			continue
		}

		// MySQL-style placeholder.
		if c == '?' {
			sb.WriteString("$?")
			continue
		}
		// Postgres-style numeric placeholder: $N where N is one or more digits.
		if c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
			sb.WriteString("$?")
			i++
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			i-- // loop will i++
			continue
		}

		// Lowercase ASCII uppercase.
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		sb.WriteByte(c)
	}

	// Collapse whitespace runs to single spaces.
	return strings.Join(strings.Fields(sb.String()), " ")
}

// Match returns true if the incoming SQL query matches the rule pattern
// after both have been run through Canonicalize. The pattern may contain
// '*' glob wildcards; they are preserved by canonicalization and applied
// via filepath.Match with prefix/suffix fallbacks (same semantics as the
// proxy's existing matchGlob helper).
//
// An empty pattern matches anything.
func Match(query, pattern string) bool {
	if pattern == "" {
		return true
	}
	cq := Canonicalize(query)
	cp := Canonicalize(pattern)
	return matchGlob(cq, cp)
}

// matchGlob mirrors the proxy package's internal helper so sqlmatch has no
// upward dependency. Behavior intentionally matches proxy.matchGlob.
func matchGlob(actual, pattern string) bool {
	if pattern == "" {
		return true
	}
	if strings.Contains(pattern, "*") {
		if matched, err := filepath.Match(pattern, actual); err == nil && matched {
			return true
		}
		if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(actual, strings.TrimSuffix(pattern, "*"))
		}
		if strings.HasPrefix(pattern, "*") {
			return strings.HasSuffix(actual, strings.TrimPrefix(pattern, "*"))
		}
		return false
	}
	return actual == pattern
}
