package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestMonitor_OnMatcherFiltersEvents — events that don't match the
// `on=` matcher do not invoke the update/check callbacks at all.
func TestMonitor_OnMatcherFiltersEvents(t *testing.T) {
	rt := New(testLogger())
	calls := 0
	check := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		calls++
		return starlark.True, nil
	})
	m := &MonitorDef{
		Name:  "filter_test",
		On:    &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "target" }},
		Check: check,
	}
	rt.RegisterMonitor(m)

	rt.events.Emit("noise", "svc", nil) // ignored
	rt.events.Emit("target", "svc", nil) // matches
	rt.events.Emit("other", "svc", nil)  // ignored

	if calls != 1 {
		t.Errorf("expected check to fire 1 time, got %d", calls)
	}
}

// TestMonitor_StateThreadingAcrossEvents — update returns a new state
// that the next update sees. Verifies the state cell is being read
// and written correctly.
func TestMonitor_StateThreadingAcrossEvents(t *testing.T) {
	rt := New(testLogger())
	var lastSeenState starlark.Value

	update := starlark.NewBuiltin("update", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		// state is args[1]; return state + 1.
		var n int
		if v, ok := args[1].(starlark.Int); ok {
			n64, _ := v.Int64()
			n = int(n64)
		}
		return starlark.MakeInt(n + 1), nil
	})
	check := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		lastSeenState = args[1]
		return starlark.True, nil
	})

	m := &MonitorDef{
		Name:      "counter",
		On:        &MatcherVal{matchFn: func(Event) bool { return true }},
		StateInit: starlark.MakeInt(0),
		Update:    update,
		Check:     check,
	}
	rt.RegisterMonitor(m)

	rt.events.Emit("e", "svc", nil)
	rt.events.Emit("e", "svc", nil)
	rt.events.Emit("e", "svc", nil)

	n, _ := lastSeenState.(starlark.Int).Int64()
	if n != 3 {
		t.Errorf("expected state=3 after 3 events, got %d", n)
	}
}

// TestMonitor_StateInitDefaultsToNone — when StateInit is nil, the
// state passed to update/check is starlark.None.
func TestMonitor_StateInitDefaultsToNone(t *testing.T) {
	rt := New(testLogger())
	var seenState starlark.Value
	check := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		seenState = args[1]
		return starlark.True, nil
	})
	m := &MonitorDef{
		Name:  "none_init",
		On:    &MatcherVal{matchFn: func(Event) bool { return true }},
		Check: check,
	}
	rt.RegisterMonitor(m)
	rt.events.Emit("e", "svc", nil)
	if seenState != starlark.None {
		t.Errorf("expected None for default StateInit, got %v", seenState)
	}
}

// TestMonitor_CheckFalseRecordsViolation — check returning false records
// a monitor error that RunTest will surface.
func TestMonitor_CheckFalseRecordsViolation(t *testing.T) {
	rt := New(testLogger())
	check := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		return starlark.False, nil
	})
	m := &MonitorDef{
		Name:  "always_fails",
		On:    &MatcherVal{matchFn: func(Event) bool { return true }},
		Check: check,
	}
	rt.RegisterMonitor(m)
	rt.events.Emit("e", "svc", nil)

	rt.monitorMu.Lock()
	errs := rt.monitorErrors
	rt.monitorMu.Unlock()
	if len(errs) != 1 {
		t.Fatalf("expected 1 monitor error, got %d", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "always_fails") {
		t.Errorf("error should cite monitor name, got: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "violated") {
		t.Errorf("error should say 'violated', got: %v", errs[0])
	}
}

// TestMonitor_CheckRaiseRecordsViolation — check raising an error also
// records a violation, with the underlying error in the message.
func TestMonitor_CheckRaiseRecordsViolation(t *testing.T) {
	rt := New(testLogger())
	checkErr := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		return nil, errBoom
	})
	m := &MonitorDef{
		Name:  "raises",
		On:    &MatcherVal{matchFn: func(Event) bool { return true }},
		Check: checkErr,
	}
	rt.RegisterMonitor(m)
	rt.events.Emit("e", "svc", nil)
	rt.monitorMu.Lock()
	errs := rt.monitorErrors
	rt.monitorMu.Unlock()
	if len(errs) == 0 {
		t.Error("expected check raise to record a violation")
	}
}

// errBoom is a sentinel error for tests that want check= to raise.
var errBoom = errSimple("boom")

type errSimple string

func (e errSimple) Error() string { return string(e) }

// TestMonitor_UpdateOnlyNoCheck — a monitor with only Update is legal;
// no violations are produced because Check is the only verdict path.
func TestMonitor_UpdateOnlyNoCheck(t *testing.T) {
	rt := New(testLogger())
	updates := 0
	update := starlark.NewBuiltin("update", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		updates++
		return args[1], nil // identity
	})
	m := &MonitorDef{
		Name:   "update_only",
		On:     &MatcherVal{matchFn: func(Event) bool { return true }},
		Update: update,
	}
	rt.RegisterMonitor(m)
	rt.events.Emit("a", "svc", nil)
	rt.events.Emit("b", "svc", nil)

	if updates != 2 {
		t.Errorf("expected update to fire 2 times, got %d", updates)
	}
	rt.monitorMu.Lock()
	errs := rt.monitorErrors
	rt.monitorMu.Unlock()
	if len(errs) != 0 {
		t.Errorf("expected 0 violations (no check), got %d", len(errs))
	}
}

// TestMonitor_BuiltinRequiresName — monitor() requires the name positional.
func TestMonitor_BuiltinRequiresName(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
monitor(on=match.event(type="x"))
`)
	if err == nil {
		t.Error("expected error when monitor() is called without name")
	}
}

// TestMonitor_BuiltinRequiresOn — RFC-041 §5.4: on= is mandatory.
func TestMonitor_BuiltinRequiresOn(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
monitor("noop")
`)
	if err == nil {
		t.Error("expected error when monitor() is called without on=")
	}
}

// TestMonitor_BuiltinAcceptsPredicateAsOn — match.event(...) is the
// canonical on= form, but a plain callable also works (it gets wrapped
// in a MatcherVal via matcherOrPredFromArg).
func TestMonitor_BuiltinAcceptsPredicateAsOn(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
m = monitor("pred_on",
    on = lambda event: event.type == "x",
    check = lambda event, state: True,
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if rt.globals["m"] == nil {
		t.Error("monitor with predicate on= should construct")
	}
}

// TestMonitor_TopLevelAutoRegistersSpecWide — a top-level monitor()
// call ends up in specMonitors (verified separately in
// TestMonitorAutoRegisteredSpecWide in runtime_test.go), and is
// claimed away when handed to a fault scenario.
func TestMonitor_TopLevelClaimedByScenario(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = service("db", "/tmp/svc", interface("main", "tcp", 8080))

m = monitor("watch",
    on = match.event(type="syscall"),
    check = lambda event, state: True,
)

fa = fault_assumption("a",
    target = db,
    connect = deny("ECONNREFUSED"),
    monitors = [m],
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if len(rt.specMonitors) != 0 {
		t.Errorf("scenario should have claimed the monitor; specMonitors = %d", len(rt.specMonitors))
	}
}

// TestMonitor_SandboxRejectsFaultInUpdate — spec-load validation catches
// fault() references inside monitor lambdas (covered more deeply in
// monitor_sandbox_test.go; this verifies LoadString wires it in).
func TestMonitor_SandboxRejectsFaultInUpdate(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
m = monitor("bad",
    on = match.event(type="x"),
    update = lambda event, state: fault(svc, write=deny()),
)
`)
	if err == nil {
		t.Fatal("expected load error for fault() inside monitor update lambda")
	}
	if !strings.Contains(err.Error(), "fault") {
		t.Errorf("error should name 'fault', got: %v", err)
	}
}

// TestMonitor_StateIsPerRegistration — registering the same MonitorDef
// twice produces two independent state cells.
func TestMonitor_StateIsPerRegistration(t *testing.T) {
	rt := New(testLogger())
	captures := make([]starlark.Value, 0, 2)
	check := starlark.NewBuiltin("check", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		captures = append(captures, args[1])
		return starlark.True, nil
	})
	update := starlark.NewBuiltin("update", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		var n int64
		if v, ok := args[1].(starlark.Int); ok {
			n, _ = v.Int64()
		}
		return starlark.MakeInt64(n + 1), nil
	})
	m := &MonitorDef{
		Name:      "counter",
		On:        &MatcherVal{matchFn: func(Event) bool { return true }},
		StateInit: starlark.MakeInt(0),
		Update:    update,
		Check:     check,
	}
	// Two independent registrations of the same def.
	rt.RegisterMonitor(m)
	rt.RegisterMonitor(m)

	rt.events.Emit("e", "svc", nil)
	// Both registrations fired, each with their own state starting at 0
	// then advancing to 1 by the check call.
	if len(captures) != 2 {
		t.Fatalf("expected 2 captures (one per registration), got %d", len(captures))
	}
	for i, v := range captures {
		n, _ := v.(starlark.Int).Int64()
		if n != 1 {
			t.Errorf("capture[%d] = %d, want 1 (independent state cells)", i, n)
		}
	}
}
