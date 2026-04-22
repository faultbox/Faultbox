package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// callExpect drives an expectCallable the way fault_matrix dispatch
// does: one positional arg, no kwargs. Wraps the result/error pair so
// assertions stay readable.
func callExpect(t *testing.T, e starlark.Callable, result starlark.Value) error {
	t.Helper()
	thread := &starlark.Thread{Name: "expect-test"}
	_, err := starlark.Call(thread, e, starlark.Tuple{result}, nil)
	return err
}

// stepResult builds a dict matching the shape scenarios return today
// — mostly status_code + duration_ms + optional error. Lets us
// exercise the predicates without spinning up a real HTTP step.
func stepResult(t *testing.T, entries map[string]starlark.Value) *starlark.Dict {
	t.Helper()
	d := starlark.NewDict(len(entries))
	for k, v := range entries {
		if err := d.SetKey(starlark.String(k), v); err != nil {
			t.Fatalf("setkey: %v", err)
		}
	}
	return d
}

func makeExpect(t *testing.T, fn func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error), args starlark.Tuple) starlark.Callable {
	t.Helper()
	thread := &starlark.Thread{Name: "make"}
	out, err := fn(thread, nil, args, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	c, ok := out.(starlark.Callable)
	if !ok {
		t.Fatalf("not a callable: %T", out)
	}
	return c
}

func TestExpectSuccessAcceptsNonNilBelow500(t *testing.T) {
	e := makeExpect(t, builtinExpectSuccess, nil)
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(200),
		"duration_ms": starlark.MakeInt(12),
	})
	if err := callExpect(t, e, res); err != nil {
		t.Errorf("expected pass, got: %v", err)
	}
}

func TestExpectSuccessRejectsNone(t *testing.T) {
	e := makeExpect(t, builtinExpectSuccess, nil)
	err := callExpect(t, e, starlark.None)
	if err == nil || !strings.Contains(err.Error(), "None") {
		t.Errorf("expected None rejection, got: %v", err)
	}
}

func TestExpectSuccessRejects500(t *testing.T) {
	e := makeExpect(t, builtinExpectSuccess, nil)
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(503),
	})
	err := callExpect(t, e, res)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 rejection, got: %v", err)
	}
}

func TestExpectErrorWithinHappyPath(t *testing.T) {
	// 503 in 1500ms with a 2000ms budget — success.
	e := makeExpect(t, builtinExpectErrorWithin, starlark.Tuple{starlark.MakeInt(2000)})
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(503),
		"duration_ms": starlark.MakeInt(1500),
	})
	if err := callExpect(t, e, res); err != nil {
		t.Errorf("expected pass, got: %v", err)
	}
}

func TestExpectErrorWithinOverBudget(t *testing.T) {
	e := makeExpect(t, builtinExpectErrorWithin, starlark.Tuple{starlark.MakeInt(1000)})
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(503),
		"duration_ms": starlark.MakeInt(4500),
	})
	err := callExpect(t, e, res)
	if err == nil || !strings.Contains(err.Error(), "exceeded budget") {
		t.Errorf("expected budget violation, got: %v", err)
	}
}

func TestExpectErrorWithinSuccessIsViolation(t *testing.T) {
	// Scenario returned a non-error — the row was supposed to degrade.
	e := makeExpect(t, builtinExpectErrorWithin, starlark.Tuple{starlark.MakeInt(2000)})
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(200),
		"duration_ms": starlark.MakeInt(100),
	})
	err := callExpect(t, e, res)
	if err == nil || !strings.Contains(err.Error(), "returned success") {
		t.Errorf("expected success-is-violation, got: %v", err)
	}
}

func TestExpectHangAcceptsNoneResult(t *testing.T) {
	e := makeExpect(t, builtinExpectHang, nil)
	if err := callExpect(t, e, starlark.None); err != nil {
		t.Errorf("None should satisfy expect_hang, got: %v", err)
	}
}

func TestExpectHangRejectsValue(t *testing.T) {
	e := makeExpect(t, builtinExpectHang, nil)
	res := stepResult(t, map[string]starlark.Value{
		"status_code": starlark.MakeInt(200),
	})
	err := callExpect(t, e, res)
	if err == nil || !strings.Contains(err.Error(), "expected no return") {
		t.Errorf("expect_hang should reject non-None, got: %v", err)
	}
}

// End-to-end: fault_matrix() accepting an expect_* value as
// default_expect= without the `must be callable` error.
func TestFaultMatrixAcceptsExpectValue(t *testing.T) {
	rt := New(testLogger())
	src := `
db = service("db", "/tmp/mock",
    interface("main", "tcp", 5432),
)

def scenario():
    return {"status_code": 200, "duration_ms": 10}

refuse = fault_assumption("refuse", target=db, connect=deny("ECONNREFUSED"))

fault_matrix(
    scenarios      = [scenario],
    faults         = [refuse],
    default_expect = expect_success(),
)
`
	if err := rt.LoadString("fm.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if len(rt.faultScenarios) == 0 {
		t.Error("fault_matrix didn't produce any scenarios")
	}
}
