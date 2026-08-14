package star

import "strings"

// specStatements folds spec source into a form the syscall scanner can
// match against reliably: one entry per logical statement, with all
// whitespace removed and comments stripped.
//
// The scanner that decides which services get a seccomp filter looks for
// literal substrings like `fault(db,` and `write=deny(`. Applied to raw
// lines that is far more brittle than it appears, and it failed in two
// ways that both produced the same symptom — a fault installed in the
// spec that silently never fires, because no filter was installed at all:
//
//	fault(db, write = delay("1ms"), run = s)   # spaces around `=`
//
//	fault(                                     # call split across lines
//	    db,
//	    write = deny("EIO"),
//	    run = scenario,
//	)
//
// Both are idiomatic Starlark, and the second is what any real spec looks
// like once it has more than two arguments. Neither matched, so
// `requiredSyscallsForService` returned nothing and the service ran
// unfiltered. The declared fault then had nothing to attach to.
//
// Folding on paren depth handles the multi-line case; removing whitespace
// handles the spacing case. Both are safe for this purpose: the result is
// only ever tested with strings.Contains for call-shaped substrings, never
// executed or re-parsed.
//
// This is a heuristic, not a parser. It cannot see through a variable
// (`rules = {"write": deny("EIO")}` then `fault(db, **rules)`) or a helper
// function, and those still produce no filter. The FAULT_NOT_FIRED
// diagnostic is the backstop for what static analysis cannot reach.
func specStatements(src string) []string {
	var out []string
	var cur strings.Builder
	depth := 0

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}

	for _, line := range strings.Split(src, "\n") {
		code := stripComment(line)

		for _, r := range code {
			switch r {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
			if r == ' ' || r == '\t' || r == '\r' {
				continue
			}
			cur.WriteRune(r)
		}

		// A statement ends at a line break only when every bracket opened
		// on it has been closed. Otherwise the call continues on the next
		// line and its arguments belong to the same statement.
		if depth == 0 {
			flush()
		}
	}
	flush()

	return out
}

// stripComment removes a trailing `#` comment, ignoring `#` inside string
// literals so a spec like `http.error(body = "#nope")` keeps its argument.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}
