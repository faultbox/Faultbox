package star

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestDocumentedPacketExamplesParse extracts every packet_* call from the
// shipped docs and evaluates it.
//
// The July 2026 doc audit found five bugs hiding behind stale examples, and
// this release fixed another (events(where=e.type=="proxy") had never worked
// despite being documented). Examples that cannot run are worse than no
// examples: they are trusted.
func TestDocumentedPacketExamplesParse(t *testing.T) {
	files := []string{
		"../../docs/spec-language.md",
		"../../docs/tutorial/03-protocol-level/27-packet-faults.md",
	}
	// A packet_* call on one line, with balanced-enough parens to evaluate.
	call := regexp.MustCompile(`packet_[a-z_]+\([^\n]*\)`)

	rt := New(testLogger())
	thread := &starlark.Thread{Name: "docaudit"}
	predeclared := rt.builtins()

	var checked int
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range call.FindAllString(string(raw), -1) {
			expr := strings.TrimSuffix(strings.TrimSpace(m), ",")
			// Skip fragments that reference spec-local names or are prose.
			if strings.Contains(expr, "**match") || strings.Contains(expr, "...") {
				continue
			}
			if strings.Count(expr, "(") != strings.Count(expr, ")") {
				continue
			}
			// Examples use `lambda p:` which needs no outer scope.
			if _, err := starlark.Eval(thread, "doc.star", expr, predeclared); err != nil {
				t.Errorf("%s: documented example does not evaluate:\n  %s\n  %v", f, expr, err)
				continue
			}
			checked++
		}
	}
	if checked < 15 {
		t.Errorf("only %d documented packet examples checked; the extractor is probably broken", checked)
	}
	t.Logf("verified %d documented packet_* examples", checked)
}
