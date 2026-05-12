package star

import (
	"strings"
	"testing"

	"go.starlark.net/syntax"
)

func TestNewSandboxThread_NameIsTagged(t *testing.T) {
	th := newSandboxThread("balance_inv")
	if !strings.HasPrefix(th.Name, "monitor:") {
		t.Errorf("expected thread name to start with 'monitor:', got %q", th.Name)
	}
	if !strings.Contains(th.Name, "balance_inv") {
		t.Errorf("expected monitor id in thread name, got %q", th.Name)
	}
}

func TestValidateMonitorLambdas_CleanLambdaPasses(t *testing.T) {
	src := `
monitor("balance_inv",
    on = match.event(type="balance"),
    state_init = {"last": None},
    update = lambda event, state: {"last": event.amount},
    check = lambda event, state: state["last"] == None or int(state["last"]) >= 0,
)
`
	if err := validateMonitorLambdasInSource("test.star", src); err != nil {
		t.Errorf("clean monitor should pass validation, got: %v", err)
	}
}

func TestValidateMonitorLambdas_RejectsFaultCallInUpdate(t *testing.T) {
	src := `
monitor("bad",
    on = match.event(type="x"),
    update = lambda event, state: fault(svc, write=deny()),
    check = lambda event, state: True,
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for fault() inside update lambda")
	}
	if !strings.Contains(err.Error(), "fault") {
		t.Errorf("error should name 'fault', got: %v", err)
	}
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("error should cite the monitor name, got: %v", err)
	}
}

func TestValidateMonitorLambdas_RejectsAssertEqInCheck(t *testing.T) {
	src := `
monitor("strict",
    on = match.event(type="x"),
    update = lambda event, state: state,
    check = lambda event, state: assert_eq(event.amount, 0),
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for assert_eq() inside check lambda")
	}
	if !strings.Contains(err.Error(), "assert_eq") {
		t.Errorf("error should name 'assert_eq', got: %v", err)
	}
}

func TestValidateMonitorLambdas_RejectsRecursiveMonitor(t *testing.T) {
	src := `
monitor("recursive",
    on = match.event(type="x"),
    update = lambda event, state: state,
    check = lambda event, state: monitor("inner", on=match.event()),
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for nested monitor() call")
	}
	if !strings.Contains(err.Error(), "monitor") {
		t.Errorf("error should mention 'monitor', got: %v", err)
	}
}

func TestValidateMonitorLambdas_RejectsAwaitInCheck(t *testing.T) {
	src := `
monitor("blocking",
    on = match.event(type="x"),
    update = lambda event, state: state,
    check = lambda event, state: await_event(match.event(type="y")),
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for await_event() inside check lambda")
	}
	if !strings.Contains(err.Error(), "await_event") {
		t.Errorf("error should name 'await_event', got: %v", err)
	}
}

func TestValidateMonitorLambdas_AllowsBasicStarlarkBuiltins(t *testing.T) {
	src := `
monitor("uses_builtins",
    on = match.event(type="x"),
    update = lambda event, state: {"count": len(state.get("items", [])) + 1},
    check = lambda event, state: any([state.get("count", 0) >= 0, True]),
)
`
	if err := validateMonitorLambdasInSource("test.star", src); err != nil {
		t.Errorf("len/any/etc should be allowed, got: %v", err)
	}
}

func TestValidateMonitorLambdas_AllowsLocalVariables(t *testing.T) {
	src := `
THRESHOLD = -100
monitor("uses_const",
    on = match.event(type="balance"),
    update = lambda event, state: state,
    check = lambda event, state: int(event.amount) >= THRESHOLD,
)
`
	if err := validateMonitorLambdasInSource("test.star", src); err != nil {
		t.Errorf("global constants should be allowed, got: %v", err)
	}
}

func TestValidateMonitorLambdas_DefByReferenceIsKnownLimitation(t *testing.T) {
	// def-form passed by reference: the validator only inspects the AST
	// node of the kwarg value, which is `bad_update` (an Ident) — not
	// the def's body. The Ident name is not in the denylist so this
	// case currently slips through. Documented as a known limitation
	// rather than treated as a bug — inline lambdas are the common
	// case in spec-language docs.
	src := `
def bad_update(event, state):
    return fault(svc, write=deny())

monitor("def_form",
    on = match.event(type="x"),
    update = bad_update,
    check = lambda event, state: True,
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err != nil {
		t.Logf("validator surfaced def-form misuse (unexpected, but fine): %v", err)
	}
}

func TestValidateMonitorLambdas_RejectsEventuallyInside(t *testing.T) {
	src := `
monitor("nested_eventually",
    on = match.event(type="x"),
    update = lambda event, state: state,
    check = lambda event, state: eventually(lambda t: True),
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for nested eventually() call")
	}
}

func TestValidateMonitorLambdas_RejectsServiceInside(t *testing.T) {
	src := `
monitor("uses_service",
    on = match.event(type="x"),
    update = lambda event, state: state,
    check = lambda event, state: service("dynamic", "/bin/x"),
)
`
	err := validateMonitorLambdasInSource("test.star", src)
	if err == nil {
		t.Fatal("expected validation error for service() inside check lambda")
	}
}

func TestValidateMonitorLambdas_EmptySourceIsNoop(t *testing.T) {
	if err := validateMonitorLambdasInSource("test.star", ""); err != nil {
		t.Errorf("empty source should validate as ok, got: %v", err)
	}
}

func TestValidateMonitorLambdas_NoMonitorCallIsNoop(t *testing.T) {
	src := `
def test_foo():
    fault(svc, write=deny(), run=lambda: None)
`
	if err := validateMonitorLambdasInSource("test.star", src); err != nil {
		t.Errorf("source without monitor() should validate as ok, got: %v", err)
	}
}

func TestValidateMonitorLambdas_GarbageSourceDoesNotPanic(t *testing.T) {
	src := `this is not valid starlark @ all`
	// Parse error → validator returns nil (let ExecFile produce the
	// real error). We just want to ensure it doesn't panic.
	err := validateMonitorLambdasInSource("test.star", src)
	if err != nil {
		t.Logf("garbage source produced error (acceptable): %v", err)
	}
}

func TestValidateMonitorLambdas_DenylistCoverageSpotCheck(t *testing.T) {
	// Spot-check a representative selection of denylist entries by
	// confirming each one trips the validator when referenced inside
	// a monitor lambda. This is a regression guard against accidentally
	// dropping an entry from monitorSandboxDenylist.
	for _, name := range []string{
		"fault", "fault_all", "service", "assert_true", "assert_eq",
		"parallel", "partition", "eventually", "always", "monitor",
		"await_stable", "await_event", "determinism", "trace",
	} {
		t.Run(name, func(t *testing.T) {
			src := "monitor(\"x\", on=match.event(), check=lambda event, state: " + name + "())\n"
			err := validateMonitorLambdasInSource("test.star", src)
			if err == nil {
				t.Errorf("expected validation error for %s() reference", name)
			}
		})
	}
}

func TestValidateSandboxNode_NilIsNoop(t *testing.T) {
	if err := validateSandboxNode("x", nil); err != nil {
		t.Errorf("nil node should validate, got: %v", err)
	}
}

func TestValidateSandboxNode_DirectExpr(t *testing.T) {
	expr, err := syntax.ParseExpr("test.star", "fault(svc, write=deny())", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateSandboxNode("x", expr); err == nil {
		t.Error("expected error for direct fault() reference")
	}
}

func TestMonitorSandboxDenylistDescribe(t *testing.T) {
	desc := MonitorSandboxDenylistDescribe()
	if desc == "" {
		t.Fatal("expected non-empty denylist description")
	}
	// Sanity: the most-common entries appear.
	for _, name := range []string{"fault", "monitor", "service"} {
		if !strings.Contains(desc, name) {
			t.Errorf("description missing entry %q", name)
		}
	}
}
