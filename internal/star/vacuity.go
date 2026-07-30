package star

import (
	"fmt"
	"sort"
	"sync"

	"go.starlark.net/starlark"
)

// Vacuity detection (RFC-052 Gap 8).
//
// These diagnostics answer a question the rest of the tool does not: not "what
// went wrong" but "would this suite notice if the thing it tests were
// completely broken?"
//
// The motivating case is a real one. `testops/corpus/postgres_fault_basic.star`
// exercised a broken Postgres client in CI on every pull request from before
// v0.15.0 until v0.16.0, and passed throughout:
//
//	env = {"POSTGRES_HOST_AUTH_METHOD": "trust", ...}   // credential path removed
//
//	resp = pg.main.query(sql = "SELECT 1")
//	assert_true(not resp.ok, "expected failed query under injected fault")
//
// It asserts the query *fails*. A client that cannot connect at all satisfies
// that identically to the injected fault it was written for. The suite had no
// positive control — nothing anywhere asserted that a Postgres step succeeds —
// so two credential bugs lived behind a green CI badge until a spec was written
// that asserted success.

// assertionBuiltins is the set of builtins whose invocation counts as "this
// test asserted something".
//
// Deliberately a name set applied by wrapping at registration, rather than a
// counter call edited into each builtin's body: a new assertion builtin that
// forgets to increment would silently make every test using it look vacuous,
// and the failure would present as a false diagnostic rather than a missing
// one. TestAssertionBuiltinsAllExist fails if a name here stops existing, which
// catches the rename case too — it caught three wrong entries on its first run.
var assertionBuiltins = map[string]bool{
	"assert_true":         true,
	"assert_eq":           true,
	"assert_eventually":   true,
	"assert_never":        true,
	"assert_before":       true,
	"expect_success":      true,
	"expect_error_within": true,
	"expect_hang":         true,
	// Note: test_panics / test_b_panics / test_panic_body are Go test fixtures
	// in this package, not spec builtins. TestAssertionBuiltinsAllExist caught
	// them here on the first run — which is what that gate is for.
	//
	// Deliberately absent: eventually / always. They are assertions, but they
	// are counted at registerExpectation() instead — the one place all three
	// registration paths converge, including test(expect=...), which never
	// passes through a builtin. Counting them here as well would double the
	// number reported to agents in TestResult.Assertions.
}

// vacuityState is the per-run bookkeeping behind the Gap 8 diagnostics.
//
// Suite-scoped on purpose. NO_POSITIVE_CONTROL is not a property of any single
// test — a fault-injection test asserting that a step fails is correct and
// normal. What is wrong is a *suite* in which that is the only kind of
// assertion an interface ever receives, and no per-test check can see that.
type vacuityState struct {
	mu sync.Mutex

	// assertions counts assertion-builtin invocations for the running test.
	assertions map[string]int

	// stepped records every service interface a step was executed against,
	// so the diagnostic only fires for interfaces the suite actually uses.
	stepped map[ifaceRef]bool

	// candidates holds interfaces that saw a *successful* step during the
	// currently-running test. Promoted to positiveControl only if that test
	// ends up passing with at least one assertion — otherwise a vacuous test
	// would silently supply the control the suite is missing.
	candidates map[ifaceRef]bool

	// positiveControl records interfaces for which some passing, asserting
	// test observed a step succeed.
	positiveControl map[ifaceRef]bool
}

// ifaceRef identifies a service interface across the whole run.
type ifaceRef struct {
	Service   string
	Interface string
}

func (r ifaceRef) String() string { return r.Service + "." + r.Interface }

func newVacuityState() *vacuityState {
	return &vacuityState{
		assertions:      map[string]int{},
		stepped:         map[ifaceRef]bool{},
		candidates:      map[ifaceRef]bool{},
		positiveControl: map[ifaceRef]bool{},
	}
}

// noteAssertion records that the named test evaluated an assertion.
func (v *vacuityState) noteAssertion(test string) {
	if test == "" {
		return // top-level or setup code; not attributable to a test
	}
	v.mu.Lock()
	v.assertions[test]++
	v.mu.Unlock()
}

// assertionCount reports how many assertions a test evaluated.
func (v *vacuityState) assertionCount(test string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.assertions[test]
}

// noteStep records a step execution against an interface.
//
// A successful step is what makes a positive control possible: it is ground
// truth from the wire that the client connected, authenticated and executed.
//
// Note there is no "and no fault was active" condition, which the
// implementation plan originally called for. That condition was wrong. If a
// step *succeeded*, the client demonstrably worked, whether or not a fault was
// installed — a fault that was installed and did not fire is a separate
// problem, and FAULT_NOT_FIRED already reports it. Requiring fault-free
// success would only make the diagnostic miss real positive controls.
func (v *vacuityState) noteStep(svc, iface string, success bool) {
	if svc == "" {
		return
	}
	ref := ifaceRef{Service: svc, Interface: iface}
	v.mu.Lock()
	v.stepped[ref] = true
	if success {
		v.candidates[ref] = true
	}
	v.mu.Unlock()
}

// endTest resolves the finished test's candidate positive controls.
//
// The gate is "passed, and asserted at least once". The step result alone
// proves the client works; the assertion is what makes the suite *notice* when
// it stops working. A suite whose only successful steps sit inside tests that
// assert nothing would not fail if the client broke, so it has no positive
// control in the sense that matters.
func (v *vacuityState) endTest(test, result string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	confirming := result == "pass" && v.assertions[test] > 0
	for ref := range v.candidates {
		if confirming {
			v.positiveControl[ref] = true
		}
		delete(v.candidates, ref)
	}
}

// missingPositiveControl lists stepped interfaces that no passing, asserting
// test ever observed succeed, in deterministic order.
func (v *vacuityState) missingPositiveControl() []ifaceRef {
	v.mu.Lock()
	defer v.mu.Unlock()

	var out []ifaceRef
	for ref := range v.stepped {
		if !v.positiveControl[ref] {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Interface < out[j].Interface
	})
	return out
}

// suiteDiagnostics builds the run-level Gap 8 findings.
func (v *vacuityState) suiteDiagnostics() []Diagnostic {
	var diags []Diagnostic
	for _, ref := range v.missingPositiveControl() {
		diags = append(diags, Diagnostic{
			Level:   "warning",
			Code:    "NO_POSITIVE_CONTROL",
			Message: fmt.Sprintf("no test asserts that a step on '%s' succeeds", ref),
			Suggestion: fmt.Sprintf(
				"Every assertion about '%s' in this suite is satisfied by a client that cannot "+
					"connect at all, so the suite would not fail if it broke. Add one test that "+
					"runs a step against '%s' with no fault injected and asserts r.ok is True.",
				ref, ref),
			Service: ref.Service,
		})
	}
	return diags
}

// wrapAssertionBuiltins returns builtins with every assertion-family entry
// wrapped so its invocation is counted.
//
// Wrapping preserves the builtin's name, so error messages and tracebacks are
// unchanged — the wrapper is invisible to specs.
func (rt *Runtime) wrapAssertionBuiltins(b starlark.StringDict) starlark.StringDict {
	for name := range assertionBuiltins {
		orig, ok := b[name].(*starlark.Builtin)
		if !ok {
			continue // validated by test, not silently tolerated at runtime
		}
		fn := orig
		b[name] = starlark.NewBuiltin(name, func(
			thread *starlark.Thread, _ *starlark.Builtin,
			args starlark.Tuple, kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			rt.vacuity.noteAssertion(rt.currentTestName)
			return fn.CallInternal(thread, args, kwargs)
		})
	}
	return b
}
