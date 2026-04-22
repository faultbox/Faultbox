package star

import (
	"fmt"

	"go.starlark.net/starlark"
)

// RFC-027: three outcome-predicate builtins for fault_matrix rows.
// Each returns a starlark.Callable that the existing fs.Expect
// dispatch invokes with (scenario_result). On mismatch the callable
// returns an error which the runtime surfaces as a row failure.
//
//   expect_success()            — result is non-nil; status_code<500.
//   expect_error_within(ms=…)   — result indicates error AND stayed
//                                 within the SLA budget.
//   expect_hang()               — scenario didn't return (timed out).
//
// These are deliberately callables (not a new Expectation interface)
// so they drop straight into `default_expect=`/`overrides={}` without
// touching runtime.go's fault_matrix dispatch loop. Richer outcome
// taxonomy lives in v0.9.9+ once RFC-029 consumes it.

// builtinExpectSuccess implements expect_success().
func builtinExpectSuccess(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs("expect_success", args, kwargs); err != nil {
		return nil, err
	}
	return newExpect("expect_success", func(result starlark.Value) error {
		if result == nil || result == starlark.None {
			return fmt.Errorf("expect_success: scenario returned None (did it complete?)")
		}
		if code, ok := resultStatusCode(result); ok && code >= 500 {
			return fmt.Errorf("expect_success: status_code=%d (want < 500)", code)
		}
		return nil
	}), nil
}

// builtinExpectErrorWithin implements expect_error_within(ms=N).
// An error shape is either an explicit error field on the result or a
// status_code >= 500. Returning success within the SLA counts as a
// violation because the scenario was supposed to degrade.
func builtinExpectErrorWithin(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var ms int
	if err := starlark.UnpackArgs("expect_error_within", args, kwargs, "ms", &ms); err != nil {
		return nil, err
	}
	if ms <= 0 {
		return nil, fmt.Errorf("expect_error_within: ms must be > 0 (got %d)", ms)
	}
	return newExpect("expect_error_within", func(result starlark.Value) error {
		if result == nil || result == starlark.None {
			return fmt.Errorf("expect_error_within(ms=%d): scenario didn't return (hang? use expect_hang instead)", ms)
		}
		code, _ := resultStatusCode(result)
		errField, hasErr := resultErrorField(result)
		// "errored" means status>=500 or non-empty error.
		errored := code >= 500 || (hasErr && errField != "")
		if !errored {
			return fmt.Errorf("expect_error_within(ms=%d): scenario returned success (status=%d) — expected an error", ms, code)
		}
		// Duration check.
		duration, hasDur := resultDurationMs(result)
		if hasDur && duration > int64(ms) {
			return fmt.Errorf("expect_error_within(ms=%d): took %dms — exceeded budget", ms, duration)
		}
		return nil
	}), nil
}

// builtinExpectHang implements expect_hang(). Scenario didn't return —
// indicates the scenario was cancelled by its row timeout. Use for
// rows that deliberately exercise caller-timeout paths.
func builtinExpectHang(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs("expect_hang", args, kwargs); err != nil {
		return nil, err
	}
	return newExpect("expect_hang", func(result starlark.Value) error {
		if result != nil && result != starlark.None {
			return fmt.Errorf("expect_hang: scenario returned %v — expected no return", result)
		}
		return nil
	}), nil
}

// expectCallable wraps a Go predicate so the fault_matrix/fault_scenario
// dispatch can invoke it as a plain callable. Implements starlark.Callable
// for Call() and starlark.Value so Starlark tolerates it in the
// default_expect=/overrides= slots.
type expectCallable struct {
	name  string
	check func(result starlark.Value) error
}

func newExpect(name string, check func(starlark.Value) error) *expectCallable {
	return &expectCallable{name: name, check: check}
}

func (e *expectCallable) Name() string { return e.name }

func (e *expectCallable) CallInternal(thread *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var result starlark.Value = starlark.None
	if len(args) > 0 {
		result = args[0]
	}
	if err := e.check(result); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (e *expectCallable) String() string        { return fmt.Sprintf("<%s>", e.name) }
func (e *expectCallable) Type() string          { return "expectation" }
func (e *expectCallable) Freeze()               {}
func (e *expectCallable) Truth() starlark.Bool  { return starlark.True }
func (e *expectCallable) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: expectation") }

var _ starlark.Callable = (*expectCallable)(nil)

// resultStatusCode extracts a `status_code` field from the scenario
// result — typically a StepResult (HTTP) or a dict — and reports
// whether it was present. Callers use the ok bool to distinguish
// "field missing" from "field present and zero."
func resultStatusCode(v starlark.Value) (int, bool) {
	return readIntAttr(v, "status_code")
}

func resultDurationMs(v starlark.Value) (int64, bool) {
	n, ok := readIntAttr(v, "duration_ms")
	return int64(n), ok
}

func resultErrorField(v starlark.Value) (string, bool) {
	return readStringAttr(v, "error")
}

// readIntAttr looks up `name` as an attribute (struct field or dict key)
// and coerces to int. Returns ok=false when missing or non-numeric.
func readIntAttr(v starlark.Value, name string) (int, bool) {
	attr := fetchAttr(v, name)
	if attr == nil {
		return 0, false
	}
	if i, ok := attr.(starlark.Int); ok {
		n, _ := i.Int64()
		return int(n), true
	}
	return 0, false
}

func readStringAttr(v starlark.Value, name string) (string, bool) {
	attr := fetchAttr(v, name)
	if attr == nil {
		return "", false
	}
	if s, ok := attr.(starlark.String); ok {
		return string(s), true
	}
	return "", false
}

// fetchAttr looks a name up on either a struct-like value (HasAttrs
// interface) or a Starlark dict. Returns nil when not found so
// callers distinguish absence from present-but-wrong-type.
func fetchAttr(v starlark.Value, name string) starlark.Value {
	if ha, ok := v.(starlark.HasAttrs); ok {
		if got, err := ha.Attr(name); err == nil && got != nil {
			return got
		}
	}
	if d, ok := v.(*starlark.Dict); ok {
		got, _, _ := d.Get(starlark.String(name))
		return got
	}
	return nil
}
