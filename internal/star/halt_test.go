package star

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestHalt_BareReturnsSentinel(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinHalt(nil, nil, nil, nil)
	if err == nil {
		t.Fatal("halt() must return an error")
	}
	if !errors.Is(err, ErrHalt) {
		t.Errorf("err must wrap ErrHalt; got %v", err)
	}
	var he *HaltError
	if !errors.As(err, &he) {
		t.Fatalf("err must be *HaltError; got %T", err)
	}
	if he.Reason != "" {
		t.Errorf("bare halt() should have empty reason; got %q", he.Reason)
	}
}

func TestHalt_WithReason(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinHalt(nil, nil, starlark.Tuple{starlark.String("invalid combo")}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var he *HaltError
	if !errors.As(err, &he) || he.Reason != "invalid combo" {
		t.Errorf("expected Reason='invalid combo'; got %v", he)
	}
	if !strings.Contains(err.Error(), "invalid combo") {
		t.Errorf("error message should contain reason; got %q", err.Error())
	}
}

func TestHalt_RejectsKwargsAndNonString(t *testing.T) {
	rt := New(testLogger())
	_, err := rt.builtinHalt(nil, nil, starlark.Tuple{starlark.MakeInt(42)}, nil)
	if err == nil {
		t.Error("non-string reason must error")
	}
	_, err = rt.builtinHalt(nil, nil, nil, []starlark.Tuple{{starlark.String("k"), starlark.String("v")}})
	if err == nil {
		t.Error("kwargs must error")
	}
	_, err = rt.builtinHalt(nil, nil, starlark.Tuple{starlark.String("a"), starlark.String("b")}, nil)
	if err == nil {
		t.Error("two positional args must error")
	}
}

// TestHalt_RunTestRecordsHaltedOutcome ensures that a halt() call
// inside a test body lands the test as Result="halted" — NOT "fail",
// "error", or "pass" — so suite-level tallies stay clean.
func TestHalt_RunTestRecordsHaltedOutcome(t *testing.T) {
	rt := New(testLogger())
	src := `def test_halted_branch():
    halt("rare invalid combo")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_halted_branch")
	if tr.Result != "halted" {
		t.Errorf("Result = %q, want halted", tr.Result)
	}
	if !strings.Contains(tr.Reason, "rare invalid combo") {
		t.Errorf("Reason should carry the halt() argument; got %q", tr.Reason)
	}
}

func TestHalt_BareCallInTestBody(t *testing.T) {
	rt := New(testLogger())
	src := `def test_bare_halt():
    halt()
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_bare_halt")
	if tr.Result != "halted" {
		t.Errorf("Result = %q, want halted", tr.Result)
	}
}
