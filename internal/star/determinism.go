package star

import (
	"fmt"
	"strings"

	"go.starlark.net/starlark"
)

// RFC-040 determinism levels. v0.13.0 implements L0 (plan determinism, no
// runtime detection) and L1 (mediated-event determinism with unmediated_io
// detection). L2..L5 are reserved syntax — they parse but error at spec load.
const (
	DeterminismL0 = "L0"
	DeterminismL1 = "L1"
)

// Determinism runtime values.
const (
	DeterminismRuntimeDefault = "default" // seccomp-notify (Path A); caps at L1
	DeterminismRuntimeGVisor  = "gvisor"  // reserved; lands with RFC-046 Path B/C
)

// unmediated_io categories. RFC-040 §8.1 enumerates the five classes of
// unmediated I/O Faultbox can detect under L1. Spec authors list these in
// determinism(allow=...) or service(nondeterministic_ok=[...]) to tolerate
// known drift; strict mode fails the test on any category not listed.
const (
	CategoryClock             = "clock"
	CategoryRand              = "rand"
	CategoryDNS               = "dns"
	CategoryNetworkUnmediated = "network-unmediated"
	CategoryFSUnmediated      = "fs-unmediated"
)

// KnownCategories enumerates every category the determinism layer accepts in
// allow= / nondeterministic_ok= lists. fs-unmediated is reserved in v0.13.0
// — accepted in lists but no events are emitted for it yet.
var KnownCategories = []string{
	CategoryClock,
	CategoryRand,
	CategoryDNS,
	CategoryNetworkUnmediated,
	CategoryFSUnmediated,
}

func isKnownCategory(s string) bool {
	for _, c := range KnownCategories {
		if c == s {
			return true
		}
	}
	return false
}

// effectiveAllow returns the union of the spec-level allow set and the
// per-service nondeterministic_ok list. Strict mode consults this set when
// deciding whether an unmediated_io event should fail the test.
func (rt *Runtime) effectiveAllow(svcName string) map[string]bool {
	out := make(map[string]bool, len(rt.detAllow))
	for k := range rt.detAllow {
		out[k] = true
	}
	rt.mu.Lock()
	svc := rt.services[svcName]
	rt.mu.Unlock()
	if svc != nil {
		for k := range svc.NondeterministicOK {
			out[k] = true
		}
	}
	return out
}

// builtinDeterminism implements the top-level determinism() builtin from
// RFC-040 §8.4. It records the spec-level promise (level + runtime + strict
// + allow) on the Runtime and rejects any combination that v0.13.0 does not
// implement (L2..L5, runtime="gvisor"). May only be called once per spec.
func (rt *Runtime) builtinDeterminism(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if rt.detExplicit {
		return nil, fmt.Errorf("determinism(): may only be called once per spec")
	}
	if len(args) != 0 {
		return nil, fmt.Errorf("determinism() takes only keyword arguments")
	}

	level := DeterminismL1
	rtName := DeterminismRuntimeDefault
	var (
		strictV starlark.Value = starlark.None
		allowV  starlark.Value = starlark.None
	)
	if err := starlark.UnpackArgs("determinism", args, kwargs,
		"level?", &level,
		"runtime?", &rtName,
		"strict?", &strictV,
		"allow?", &allowV,
	); err != nil {
		return nil, err
	}

	switch level {
	case DeterminismL0, DeterminismL1:
		// supported
	case "L2", "L3", "L4", "L5":
		return nil, fmt.Errorf("determinism(level=%q): reserved for a future Faultbox release; v0.13.0 supports only L0 and L1 (see RFC-046 for the post-L1 roadmap)", level)
	default:
		return nil, fmt.Errorf("determinism(level=%q): must be one of L0, L1 (L2..L5 reserved)", level)
	}

	switch rtName {
	case DeterminismRuntimeDefault:
		// supported
	case DeterminismRuntimeGVisor:
		return nil, fmt.Errorf("determinism(runtime=%q): reserved for a future Faultbox release; v0.13.0 supports only runtime=%q (see RFC-046 §Path B/C)", rtName, DeterminismRuntimeDefault)
	default:
		return nil, fmt.Errorf("determinism(runtime=%q): must be %q (%q reserved)", rtName, DeterminismRuntimeDefault, DeterminismRuntimeGVisor)
	}

	// strict has no effect at L0 — there are no detection events to gate.
	// Reject the kwarg to surface the conceptual mismatch; the alternative
	// (silently ignore) hides the user's intent.
	if level == DeterminismL0 && strictV != starlark.None {
		return nil, fmt.Errorf("determinism(strict=...): has no effect at L0 (no violation events exist at L0); remove strict= or change the level to L1")
	}
	strictBool := true
	if b, ok := strictV.(starlark.Bool); ok {
		strictBool = bool(b)
	} else if strictV != starlark.None {
		return nil, fmt.Errorf("determinism(strict=...): must be a bool, got %s", strictV.Type())
	}

	allow := make(map[string]bool)
	switch v := allowV.(type) {
	case starlark.NoneType:
		// not set
	case *starlark.List:
		iter := v.Iterate()
		var item starlark.Value
		for iter.Next(&item) {
			s, ok := starlark.AsString(item)
			if !ok {
				iter.Done()
				return nil, fmt.Errorf("determinism(allow=...): list items must be strings (categories), got %s", item.Type())
			}
			if !isKnownCategory(s) {
				iter.Done()
				return nil, fmt.Errorf("determinism(allow=...): unknown category %q (known: %s)", s, strings.Join(KnownCategories, ", "))
			}
			allow[s] = true
		}
		iter.Done()
	default:
		return nil, fmt.Errorf("determinism(allow=...): must be a list of category strings, got %s", v.Type())
	}

	rt.detLevel = level
	rt.detRuntime = rtName
	rt.detStrict = strictBool
	rt.detAllow = allow
	rt.detExplicit = true
	return starlark.None, nil
}

// parseNondeterministicOK reads a service(nondeterministic_ok=[...]) kwarg
// value and returns the validated category set. Shared by builtinService.
func parseNondeterministicOK(serviceName string, val starlark.Value) (map[string]bool, error) {
	list, ok := val.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("service() %q nondeterministic_ok must be a list of category strings, got %s", serviceName, val.Type())
	}
	out := make(map[string]bool)
	iter := list.Iterate()
	defer iter.Done()
	var item starlark.Value
	for iter.Next(&item) {
		s, ok := starlark.AsString(item)
		if !ok {
			return nil, fmt.Errorf("service() %q nondeterministic_ok items must be strings, got %s", serviceName, item.Type())
		}
		if !isKnownCategory(s) {
			return nil, fmt.Errorf("service() %q nondeterministic_ok: unknown category %q (known: %s)", serviceName, s, strings.Join(KnownCategories, ", "))
		}
		out[s] = true
	}
	return out, nil
}
