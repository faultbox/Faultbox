package star

import (
	"fmt"
	"strings"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/engine"
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
//
// Holds rt.mu for the detAllow read (written by builtinDeterminism under
// the same lock). svc.NondeterministicOK is iterated after the unlock —
// it is write-once during LoadString and safe to read without a lock.
func (rt *Runtime) effectiveAllow(svcName string) map[string]bool {
	rt.mu.Lock()
	out := make(map[string]bool, len(rt.detAllow))
	for k := range rt.detAllow {
		out[k] = true
	}
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
	// LoadString runs single-threaded, so no race on detExplicit here. The
	// mu.Lock at the end protects concurrent goroutine reads of det* fields.
	if rt.detExplicit {
		return nil, fmt.Errorf("determinism(): may only be called once per spec")
	}
	// Reject positional args before UnpackArgs so they are not silently mapped
	// to the first kwarg (level). Without this guard the call determinism("L1")
	// would parse successfully with level="L1", hiding the intent to use kwargs.
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

	rt.mu.Lock()
	rt.detLevel = level
	rt.detRuntime = rtName
	rt.detStrict = strictBool
	rt.detAllow = allow
	rt.detExplicit = true
	rt.mu.Unlock()
	return starlark.None, nil
}

// strictEffective reports whether strict mode should fail the test on
// unmediated_io events. Defaults to the spec setting; overridden if the
// CLI passed --strict-determinism / --strict-determinism=false. Strict
// only applies at L1 — L0 has no detection events, L2..L5 are reserved.
//
// detStrictOverride is written once (by RunAll before any test goroutine
// starts) and treated as read-only thereafter, so the pointer dereference
// needs no lock. The det* spec fields are written under rt.mu by
// builtinDeterminism and read under the same lock here.
func (rt *Runtime) strictEffective() bool {
	if rt.detStrictOverride != nil {
		return *rt.detStrictOverride
	}
	rt.mu.Lock()
	level := rt.detLevel
	strict := rt.detStrict
	rt.mu.Unlock()
	return level == DeterminismL1 && strict
}

// firstStrictViolation walks the event log and returns the first
// unmediated_io event whose category is not in the offending service's
// effective allow set, or nil if every event is tolerated. Used by RunTest
// to fail the test with a precise, actionable error pointing at the leak.
func (rt *Runtime) firstStrictViolation(events []Event) *Event {
	for i := range events {
		ev := &events[i]
		if ev.Type != "unmediated_io" {
			continue
		}
		cat := ev.Fields["category"]
		if cat == "" {
			continue
		}
		eff := rt.effectiveAllow(ev.Service)
		if eff[cat] {
			continue
		}
		return ev
	}
	return nil
}

// strictViolationReason composes the failure string for a strict
// determinism violation. Names the category, service, syscall and call
// site, and points the user at the two escape hatches before the
// "fix the SUT" option. Single source so the wording stays consistent
// between RunTest and any future report-renderer copy.
func strictViolationReason(ev *Event) string {
	cat := ev.Fields["category"]
	syscallName := ev.Fields["syscall"]
	detail := ev.Fields["detail"]
	pid := ev.Fields["pid"]

	parts := []string{
		fmt.Sprintf("strict determinism: unmediated_io[%s] from service %q (syscall=%s, pid=%s)", cat, ev.Service, syscallName, pid),
	}
	if detail != "" {
		parts = append(parts, fmt.Sprintf("dest=%s", detail))
	}
	parts = append(parts,
		fmt.Sprintf("— add %q to determinism(allow=...) or service(%q, nondeterministic_ok=[...]) to tolerate, or fix the underlying I/O leak", cat, ev.Service),
	)
	return strings.Join(parts, " ")
}

// detectUnmediated inspects a syscall event and emits an unmediated_io event
// if it matches one of the L1 detection categories. Called from the OnSyscall
// callback. RFC-040 §8.1.
//
// Categories handled here:
//   - clock_gettime → "clock" (gettimeofday omitted: always VDSO on
//                    amd64/arm64, absent from the seccomp arch tables)
//   - getrandom     → "rand"
//   - connect       → "dns" (port 53) / "network-unmediated" (other ports
//                     not bound to a declared interface or a Faultbox proxy)
//
// fs-unmediated is a reserved category in v0.13.0 — accepted in allow= lists
// but no events are emitted yet (deferred to a future release).
//
// Q1 (reviewer): detectUnmediated fires regardless of evt.Decision, so a
// fault rule that *denies* a connect to an unmediated address still emits
// unmediated_io. This is intentional: the connect attempt itself is a
// non-determinism signal — the SUT made a choice to try to connect somewhere
// outside the declared spec topology. Whether Faultbox allowed or denied the
// call does not change the fact that the SUT's execution path diverged from
// what the spec mediates. If a caller needs to suppress detection on denied
// syscalls it can use nondeterministic_ok= on the affected service.
func (rt *Runtime) detectUnmediated(svcName string, evt engine.SyscallEvent) {
	// Fast path: skip syscalls that are never detection targets, avoiding
	// lock acquisition for the common case (write, writev, etc.).
	switch evt.Syscall {
	case "clock_gettime", "getrandom", "connect":
	default:
		return
	}
	rt.mu.Lock()
	level := rt.detLevel
	rt.mu.Unlock()
	if level != DeterminismL1 {
		return
	}
	switch evt.Syscall {
	case "clock_gettime":
		rt.emitUnmediated(svcName, CategoryClock, evt, "")
	case "getrandom":
		rt.emitUnmediated(svcName, CategoryRand, evt, "")
	case "connect":
		rt.detectUnmediatedConnect(svcName, evt)
	}
}

// detectUnmediatedConnect classifies a connect() destination. Mediated
// connections (declared interface ports, Faultbox proxy listeners) are
// silent; everything else fires either dns or network-unmediated.
func (rt *Runtime) detectUnmediatedConnect(svcName string, evt engine.SyscallEvent) {
	if evt.DestIP == "" {
		return // sockaddr read failed; nothing to classify
	}
	// Declared interface? Mediated.
	if rt.isMediatedAddress(evt.DestIP, evt.DestPort) {
		return
	}
	// Faultbox proxy listener? Mediated.
	if rt.proxyMgr != nil && rt.proxyMgr.IsListenPort(evt.DestPort) {
		return
	}
	// Port-53 → DNS heuristic. Catches plain DNS to a real resolver; misses
	// DoH/DoT (acknowledged in docs).
	if evt.DestPort == 53 {
		rt.emitUnmediated(svcName, CategoryDNS, evt, fmt.Sprintf("%s:%d", evt.DestIP, evt.DestPort))
		return
	}
	rt.emitUnmediated(svcName, CategoryNetworkUnmediated, evt, fmt.Sprintf("%s:%d", evt.DestIP, evt.DestPort))
}

// isMediatedAddress reports whether (ip, port) matches a declared interface
// port on any service in the spec — i.e. traffic Faultbox is mediating
// directly. Both container HostPort and the in-spec Port match (containers
// publish a HostPort that the SUT may dial; binary services dial Port).
//
// Known limitation (v0.13.0): matching is port-only. The ip argument is
// accepted for API forward-compatibility but not consulted. A SUT connecting
// to any host on a declared port is treated as mediated — a false negative
// for connects to hosts that happen to share the same port number but are
// not Faultbox-declared dependencies. Documented in docs/determinism.md §Known Limitations.
func (rt *Runtime) isMediatedAddress(ip string, port int) bool {
	if port <= 0 {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, svc := range rt.services {
		for _, iface := range svc.Interfaces {
			if iface.Port == port || iface.HostPort == port {
				return true
			}
		}
	}
	return false
}

// emitUnmediated writes an unmediated_io event into the event log with a
// stable field schema: category + syscall + pid + (optional) detail. The
// strict-mode check in PR 3 will read these fields to decide whether to
// fail the test.
func (rt *Runtime) emitUnmediated(svcName, category string, evt engine.SyscallEvent, detail string) {
	fields := map[string]string{
		"category": category,
		"syscall":  evt.Syscall,
		"pid":      fmt.Sprintf("%d", evt.PID),
	}
	if detail != "" {
		fields["detail"] = detail
	}
	rt.events.Emit("unmediated_io", svcName, fields)
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
