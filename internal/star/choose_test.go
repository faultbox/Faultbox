package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestChoose_ReturnsFirstOption(t *testing.T) {
	rt := New(testLogger())
	src := `r = choose([7, 8, 9])
n = nondet()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	// `choose([7,8,9])` returns 7 in rc1 single-leaf mode.
	if got := rt.globals["r"]; got != starlark.MakeInt(7) {
		t.Errorf("r = %v, want 7 (first option)", got)
	}
	// `nondet()` is sugar for choose([True, False]); first option is True.
	if got := rt.globals["n"]; got != starlark.True {
		t.Errorf("n = %v, want True (nondet first option)", got)
	}
}

func TestChoose_RecordsCallSites(t *testing.T) {
	rt := New(testLogger())
	src := `
a = choose([1, 2, 3])
b = choose("retries", [0, 1])
c = nondet()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	choices := rt.Choices()
	if len(choices) != 3 {
		t.Fatalf("expected 3 recorded choices, got %d", len(choices))
	}
	if len(choices[0].Options) != 3 {
		t.Errorf("first choice option count = %d, want 3", len(choices[0].Options))
	}
	if choices[1].Name != "retries" {
		t.Errorf("named choice Name = %q, want retries", choices[1].Name)
	}
	if len(choices[2].Options) != 2 {
		t.Errorf("nondet choice option count = %d, want 2", len(choices[2].Options))
	}
}

func TestChoose_EmptyListErrors(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose([])`)
	if err == nil {
		t.Fatal("expected error for empty options list")
	}
}

func TestChoose_NonListErrors(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose(42)`)
	if err == nil {
		t.Fatal("expected error when options is not a list")
	}
}

func TestChoose_BadArity(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose()`)
	if err == nil {
		t.Fatal("expected error for zero-arg choose()")
	}
}

func TestChoose_NamedFormStringRequired(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `x = choose(42, [1, 2])`)
	if err == nil {
		t.Fatal("expected error when name arg is not a string")
	}
}

// Review B2: nondet(svc=x) must not silently bypass the
// service-exclusion path by taking the zero-arg boolean route.
// Reject kwargs explicitly so the call surface stays unambiguous.
func TestNondet_RejectsKwargs(t *testing.T) {
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox", cmd=["sh","-c","sleep 1"])
nondet(svc=svc)
`
	err := rt.LoadString("spec.star", src)
	if err == nil {
		t.Fatal("expected error for nondet(svc=...)")
	}
	if !strings.Contains(err.Error(), "keyword arguments are not accepted") {
		t.Errorf("error should mention kwargs are rejected; got %v", err)
	}
	if rt.nondetServices["svc"] {
		t.Error("svc must NOT be marked nondet when call errored")
	}
}

// Pre-RFC-043 nondet(service) variant must continue to work — covered
// implicitly by existing tutorial specs; pin the behavior here.
func TestNondet_ServiceFormStillMarksService(t *testing.T) {
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox", cmd=["sh","-c","sleep 1"])
nondet(svc)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if !rt.nondetServices["svc"] {
		t.Errorf("expected svc to be marked nondet, got %v", rt.nondetServices)
	}
}

func TestChoiceVal_StringAndType(t *testing.T) {
	c := &ChoiceVal{Name: "x", Options: []starlark.Value{starlark.MakeInt(1), starlark.MakeInt(2)}}
	if c.Type() != "choice" {
		t.Errorf("Type = %q, want choice", c.Type())
	}
	if c.String() != "<choice x [2]>" {
		t.Errorf("String = %q", c.String())
	}
	if c.Truth() != starlark.True {
		t.Error("non-empty Choice should be truthy")
	}
	empty := &ChoiceVal{}
	if empty.Truth() != starlark.False {
		t.Error("empty Choice should be falsy")
	}
	if empty.FirstOption() != starlark.None {
		t.Error("empty Choice FirstOption should be None")
	}
}
