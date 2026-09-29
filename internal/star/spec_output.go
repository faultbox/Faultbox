package star

import (
	"fmt"
	"os"
	"regexp"
	"sync"

	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
)

const specOutputKey = "faultbox.specOutput"
const specOutputSuppressedKey = "faultbox.specOutputSuppressed"

// A capability bound to one test run. Closing it prevents late callbacks from
// writing into the next test's trace after EventLog.Reset.
type specOutputRun struct {
	mu     sync.RWMutex
	active bool
	test   string
	events *EventLog
}

type specOutputContext struct {
	run            *specOutputRun
	phase, service string
}

func (rt *Runtime) beginSpecOutput(name string) func() {
	run := &specOutputRun{active: true, test: name, events: rt.events}
	rt.outputMu.Lock()
	rt.outputRun = run
	rt.outputMu.Unlock()
	return func() {
		run.mu.Lock()
		run.active = false
		run.mu.Unlock()
		rt.outputMu.Lock()
		if rt.outputRun == run {
			rt.outputRun = nil
		}
		rt.outputMu.Unlock()
	}
}

func (rt *Runtime) newSpecThread(name, phase, service string) *starlark.Thread {
	rt.outputMu.RLock()
	run := rt.outputRun
	rt.outputMu.RUnlock()
	t := &starlark.Thread{Name: name, Print: printSpecOutput}
	if run != nil {
		t.SetLocal(specOutputKey, &specOutputContext{run: run, phase: phase, service: service})
	}
	return t
}

// Children inherit capabilities, never acquire them from the current runtime.
// This also prevents an aliased parallel() in a predicate from enabling emit().
func childSpecThread(name string, parents ...*starlark.Thread) *starlark.Thread {
	t := &starlark.Thread{Name: name, Print: printSpecOutput}
	if len(parents) > 0 && parents[0] != nil && parents[0].Local(specOutputSuppressedKey) != true {
		t.SetLocal(specOutputKey, parents[0].Local(specOutputKey))
	}
	return t
}

func suppressSpecOutput(t *starlark.Thread) func() {
	previous := t.Local(specOutputSuppressedKey)
	t.SetLocal(specOutputSuppressedKey, true)
	return func() { t.SetLocal(specOutputSuppressedKey, previous) }
}

func recordSpecOutput(t *starlark.Thread, typ string, fields map[string]string) error {
	output, _ := t.Local(specOutputKey).(*specOutputContext)
	if output == nil || t.Local(specOutputSuppressedKey) == true {
		return fmt.Errorf("emit() is only available in test bodies, setup, seed/reset callbacks and their parallel branches; not in predicates or mock handlers")
	}
	run := output.run
	run.mu.RLock()
	defer run.mu.RUnlock()
	if !run.active {
		return fmt.Errorf("emit(): test run has ended")
	}
	fields["test"], fields["phase"], fields["thread"] = run.test, output.phase, t.Name
	file, line := callerPosition(t)
	fields["source"], fields["line"] = file, fmt.Sprint(line)
	run.events.Emit(typ, output.service, fields)
	return nil
}

func printSpecOutput(t *starlark.Thread, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	// Prints from predicates stay on the console: emitting another event during
	// synchronous predicate dispatch would recursively evaluate that predicate.
	_ = recordSpecOutput(t, "spec.stdout", map[string]string{"message": msg})
}

var customEventType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)

func (rt *Runtime) builtinEmit(t *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var typ string
	var data starlark.Value
	if err := starlark.UnpackArgs("emit", args, kwargs, "type", &typ, "data", &data); err != nil {
		return nil, err
	}
	if !customEventType.MatchString(typ) {
		return nil, fmt.Errorf("emit(): type must start with a letter and contain only letters, digits, '.', '_' or '-' (max 128 characters)")
	}
	encoded, err := starlark.Call(t, starlarkjson.Module.Members["encode"].(starlark.Callable), starlark.Tuple{data}, nil)
	if err != nil {
		return nil, fmt.Errorf("emit(): %w", err)
	}
	// Snapshot as JSON before returning; later caller mutation cannot alter evidence.
	if err := recordSpecOutput(t, "custom."+typ, map[string]string{"data": string(encoded.(starlark.String))}); err != nil {
		return nil, err
	}
	return starlark.None, nil
}
