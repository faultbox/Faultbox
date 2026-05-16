package star

import (
	"errors"
	"fmt"

	"go.starlark.net/starlark"
)

// ErrHalt is the sentinel error returned by the halt() builtin.
// RunTest matches it via errors.Is so the test result lands as
// Result="halted" (not "fail" or "error"). RFC-043 §5.3.
//
// Starlark wraps builtin errors in *starlark.EvalError; the wrapper
// preserves Unwrap(), so errors.Is(err, ErrHalt) correctly traverses
// the stack-trace wrapping.
var ErrHalt = errors.New("halt")

// HaltError carries the optional human-readable reason a halt()
// call can attach. Wraps ErrHalt so the sentinel match still works.
type HaltError struct {
	Reason string
}

func (e *HaltError) Error() string {
	if e.Reason == "" {
		return "halt"
	}
	return "halt: " + e.Reason
}
func (e *HaltError) Unwrap() error { return ErrHalt }

// builtinHalt implements RFC-043 §5.3. Body execution stops at the
// halt() call site; the test result is recorded as "halted" with the
// optional reason attached.
//
//	halt()                  # bare halt
//	halt("invalid combo")   # halt with a reason rendered in the report
func (rt *Runtime) builtinHalt(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) > 0 {
		return nil, fmt.Errorf("halt() takes only an optional positional reason")
	}
	switch len(args) {
	case 0:
		return nil, &HaltError{}
	case 1:
		reason, ok := starlark.AsString(args[0])
		if !ok {
			return nil, fmt.Errorf("halt() reason must be a string, got %s", args[0].Type())
		}
		return nil, &HaltError{Reason: reason}
	}
	return nil, fmt.Errorf("halt() takes at most one argument; got %d", len(args))
}
