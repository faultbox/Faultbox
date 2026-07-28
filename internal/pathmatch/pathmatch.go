// Package pathmatch implements the glob dialect Faultbox uses for filesystem
// path targeting.
//
// It exists because `filepath.Match` — what the fault engine used until
// v0.14.0 — cannot cross a path separator. `/data/*` matches `/data/foo` but
// not `/data/a/b`, so a rule written as `op(path="/data/*")` against a
// database that nests its files silently matched nothing: no fault fired, no
// diagnostic, and the test passed. The source comment on the old matcher read
// "For the PoC this is fine."
//
// The dialect is the familiar one:
//
//	"*"      matches any run of characters except '/'
//	"**"     matches any run of characters including '/'
//	"?"      matches exactly one character except '/'
//	"[abc]"  character class, with [!abc] / [^abc] negation and a-z ranges
//	"\\x"    escapes the next character
//
// A trailing `/**` also matches the directory itself, so `/data/**` matches
// `/data`, `/data/x` and `/data/a/b/c`. Anything else is a literal.
package pathmatch

import "strings"

// Match reports whether path satisfies pattern.
//
// An empty pattern matches everything — callers use "" to mean "no path
// filter", and the fault engine relies on that.
func Match(pattern, path string) bool {
	if pattern == "" {
		return true
	}
	return matchHere(pattern, path)
}

// matchHere walks pattern and path together. Recursion happens only at `**`
// and at a `*` that must try successive split points, so ordinary patterns
// walk in a single pass.
func matchHere(pat, s string) bool {
	for len(pat) > 0 {
		// A trailing "/**" also matches the directory itself, so "/data/**"
		// matches "/data". Checked before the literal branch below, which
		// would otherwise fail on the '/' with nothing left of the path.
		if pat == "/**" && s == "" {
			return true
		}
		switch pat[0] {
		case '*':
			// `**` crosses separators; a single `*` does not.
			if len(pat) > 1 && pat[1] == '*' {
				rest := pat[2:]
				// `/**` at the end also matches the directory itself:
				// "/data/**" matches "/data".
				if rest == "" {
					return true
				}
				// Skip a separator immediately after `**` so "a/**/b" can
				// match "a/b" as well as "a/x/b".
				if rest[0] == '/' {
					if matchHere(rest[1:], s) {
						return true
					}
				}
				for i := 0; i <= len(s); i++ {
					if matchHere(rest, s[i:]) {
						return true
					}
				}
				return false
			}
			rest := pat[1:]
			// Try every split point up to the next separator.
			for i := 0; i <= len(s); i++ {
				if matchHere(rest, s[i:]) {
					return true
				}
				if i < len(s) && s[i] == '/' {
					break
				}
			}
			return false

		case '?':
			if len(s) == 0 || s[0] == '/' {
				return false
			}
			pat, s = pat[1:], s[1:]

		case '[':
			if len(s) == 0 || s[0] == '/' {
				return false
			}
			n, ok := matchClass(pat, s[0])
			if !ok {
				return false
			}
			pat, s = pat[n:], s[1:]

		case '\\':
			if len(pat) < 2 {
				return false // trailing backslash matches nothing
			}
			if len(s) == 0 || s[0] != pat[1] {
				return false
			}
			pat, s = pat[2:], s[1:]

		default:
			if len(s) == 0 || s[0] != pat[0] {
				return false
			}
			pat, s = pat[1:], s[1:]
		}
	}
	return len(s) == 0
}

// matchClass evaluates a [...] class against c, returning how many bytes of
// the pattern it consumed.
func matchClass(pat string, c byte) (consumed int, matched bool) {
	i := 1 // skip '['
	negate := false
	if i < len(pat) && (pat[i] == '!' || pat[i] == '^') {
		negate = true
		i++
	}
	found := false
	first := true
	for i < len(pat) && (pat[i] != ']' || first) {
		first = false
		if pat[i] == '\\' && i+1 < len(pat) {
			i++
			if pat[i] == c {
				found = true
			}
			i++
			continue
		}
		// Range: a-z. The '-' is literal when it ends the class.
		if i+2 < len(pat) && pat[i+1] == '-' && pat[i+2] != ']' {
			if pat[i] <= c && c <= pat[i+2] {
				found = true
			}
			i += 3
			continue
		}
		if pat[i] == c {
			found = true
		}
		i++
	}
	if i >= len(pat) {
		return 0, false // unterminated class matches nothing
	}
	i++ // consume ']'
	return i, found != negate
}

// HasWildcard reports whether a pattern contains any glob metacharacter.
// Callers use it to skip matching work for literal paths.
func HasWildcard(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// MatchAny reports whether path satisfies any of the patterns. An empty list
// matches everything, mirroring an empty pattern.
func MatchAny(patterns []string, path string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}
