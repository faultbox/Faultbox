package star

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// RFC-052 Gap 2 — the machine-readable error taxonomy.

// The M3 gate. A code without a suggestion is a code the reader has to guess
// about, which defeats the point of having codes. Enforced here rather than by
// reviewer diligence, because "did you remember the suggestion" is exactly the
// kind of thing review misses.
func TestEveryCodeHasASuggestion(t *testing.T) {
	codes := AllCodes()
	if len(codes) == 0 {
		t.Fatal("no codes registered")
	}
	for _, c := range codes {
		s := c.Suggestion()
		if strings.TrimSpace(s) == "" {
			t.Errorf("%s has no suggestion — every code must tell the reader what to do next", c)
			continue
		}
		// A suggestion that merely restates the code is not a next move.
		if strings.EqualFold(s, string(c)) {
			t.Errorf("%s: suggestion just repeats the code", c)
		}
		if len(s) < 40 {
			t.Errorf("%s: suggestion is %d chars — too short to be actionable: %q", c, len(s), s)
		}
	}
}

// Codes are an API: agents branch on them. A typo'd or renamed code silently
// breaks that, so the constant and its registry entry must agree.
func TestEveryDeclaredConstantIsRegistered(t *testing.T) {
	declared := []Code{
		CodeSpecSyntax, CodeSpecForbiddenLambda, CodeSpecLoadFailed, CodeSpecRecipeNotFound,
		CodeHealthcheckTimeout, CodeLaunchFailed, CodeDockerUnavailable, CodeTraceHostNotRegistered,
	}
	registered := map[Code]bool{}
	for _, c := range AllCodes() {
		registered[c] = true
	}
	for _, c := range declared {
		if !registered[c] {
			t.Errorf("%s is declared as a constant but has no suggestion registered", c)
		}
	}
	if len(declared) != len(AllCodes()) {
		t.Errorf("%d constants vs %d registered codes — one list has drifted",
			len(declared), len(AllCodes()))
	}
}

// Codes must be screaming snake case: they appear in JSON and agents match on
// them, so casing drift is a compatibility break.
func TestCodeNamingConvention(t *testing.T) {
	for _, c := range AllCodes() {
		s := string(c)
		if s != strings.ToUpper(s) {
			t.Errorf("%s is not upper case", c)
		}
		if strings.ContainsAny(s, " -.") {
			t.Errorf("%s contains a separator other than underscore", c)
		}
	}
}

func TestClassifyExtractsTheCode(t *testing.T) {
	err := codedf(CodeHealthcheckTimeout, "service %q not ready: %w", "pg", errors.New("deadline exceeded"))
	got, ok := Classify(err)
	if !ok {
		t.Fatal("Classify found no code on a coded error")
	}
	if got != CodeHealthcheckTimeout {
		t.Errorf("code = %s, want %s", got, CodeHealthcheckTimeout)
	}
}

// The code must survive further wrapping, since errors travel up through
// several layers before anyone reads them.
func TestClassifySeesThroughWrapping(t *testing.T) {
	inner := codedf(CodeDockerUnavailable, "docker client: %w", errors.New("no socket"))
	wrapped := fmt.Errorf("start services: %w", fmt.Errorf("launch: %w", inner))

	got, ok := Classify(wrapped)
	if !ok {
		t.Fatal("code lost through two layers of wrapping")
	}
	if got != CodeDockerUnavailable {
		t.Errorf("code = %s, want %s", got, CodeDockerUnavailable)
	}
}

// An uncoded error must report honestly rather than guessing. A gap in the
// taxonomy is discoverable; a wrong code is something an agent acts on.
func TestClassifyDoesNotGuess(t *testing.T) {
	if c, ok := Classify(errors.New("service \"pg\" not ready: timeout")); ok {
		t.Errorf("Classify invented %s for an uncoded error whose text looks like a known one", c)
	}
	if _, ok := Classify(nil); ok {
		t.Error("Classify found a code on a nil error")
	}
}

// The original message must survive, or the code trades one kind of
// information for another instead of adding to it.
func TestCodedErrorPreservesMessageAndChain(t *testing.T) {
	sentinel := errors.New("connection refused")
	err := codedf(CodeLaunchFailed, "launch container %q: %w", "pg", sentinel)

	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("wrapped message lost: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "pg") {
		t.Errorf("context lost: %q", err.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is broken — the chain must survive coding")
	}
}

func TestCodedIgnoresNil(t *testing.T) {
	if err := coded(CodeSpecSyntax, nil); err != nil {
		t.Errorf("coded(nil) = %v, want nil", err)
	}
}

func TestDiagnosticForCodedError(t *testing.T) {
	err := codedf(CodeHealthcheckTimeout, "service %q not ready", "pg")
	d := DiagnosticFor(err)
	if d == nil {
		t.Fatal("no diagnostic for a coded error")
	}
	if d.Code != string(CodeHealthcheckTimeout) {
		t.Errorf("code = %q", d.Code)
	}
	if d.Level != "error" {
		t.Errorf("level = %q, want error", d.Level)
	}
	if d.Suggestion == "" {
		t.Error("diagnostic carries no suggestion")
	}
	if !strings.Contains(d.Message, "pg") {
		t.Errorf("message lost context: %q", d.Message)
	}
}

func TestDiagnosticForUncodedErrorIsNil(t *testing.T) {
	if d := DiagnosticFor(errors.New("something went wrong")); d != nil {
		t.Errorf("invented a diagnostic for an uncoded error: %+v", d)
	}
}

// A genuine syntax error and a spec that parsed then failed are different
// problems with different fixes, so they must not share a code.
func TestSpecSyntaxIsDistinguishedFromLoadFailure(t *testing.T) {
	rt := New(testLogger())

	// Not parseable: positional after keyword is a Starlark syntax error.
	err := rt.LoadString("bad.star", "service(name = \"x\", \"positional\")\n")
	if err == nil {
		t.Fatal("expected a load error")
	}
	if c, _ := Classify(err); c != CodeSpecSyntax {
		t.Errorf("syntax error classified as %s, want %s (%v)", c, CodeSpecSyntax, err)
	}

	// Parses fine, fails during execution: unknown kwarg.
	rt2 := New(testLogger())
	err2 := rt2.LoadString("bad2.star", "service(\"x\", nonsense_kwarg = 1)\n")
	if err2 == nil {
		t.Fatal("expected a load error for an unknown kwarg")
	}
	if c, _ := Classify(err2); c != CodeSpecLoadFailed {
		t.Errorf("exec-time failure classified as %s, want %s (%v)", c, CodeSpecLoadFailed, err2)
	}
}

// The sandbox denylist has its own code, because the fix is specific: signal
// through the return value rather than calling an assertion.
func TestForbiddenLambdaHasItsOwnCode(t *testing.T) {
	rt := New(testLogger())
	// A lambda calling fault() — the shape monitor_sandbox_test.go uses as its
	// canonical rejection. My first attempt at this test used
	// on = "step_recv", which is not a valid matcher, so it never reached the
	// sandbox at all and asserted nothing about the code under test.
	src := `
monitor("bad",
    on = match.event(type="x"),
    update = lambda event, state: fault(svc, write=deny()),
    check = lambda event, state: True,
)
`
	err := rt.LoadString("m.star", src)
	if err == nil {
		t.Fatal("expected the monitor sandbox to reject fault() inside update=")
	}
	if c, _ := Classify(err); c != CodeSpecForbiddenLambda {
		t.Errorf("sandbox violation classified as %s, want %s (%v)", c, CodeSpecForbiddenLambda, err)
	}
}

// An inner code must survive ExecFile's wrapping. A missing @faultbox/ recipe
// reported SPEC_LOAD_FAILED with an irrelevant suggestion about keyword
// arguments, because specLoadCode overwrote the precise code raised inside the
// load hook with its own generic one.
func TestInnerCodeSurvivesLoadWrapping(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("r.star", `load("@faultbox/recipes/definitely-not-a-recipe.star", "x")`+"\n")
	if err == nil {
		t.Fatal("expected a load error for a missing recipe")
	}
	if c, _ := Classify(err); c != CodeSpecRecipeNotFound {
		t.Errorf("missing recipe classified as %s, want %s — the outer wrapper overwrote it (%v)",
			c, CodeSpecRecipeNotFound, err)
	}
}
