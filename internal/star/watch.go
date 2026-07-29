package star

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/gvisor/seccheck"
	"github.com/faultbox/Faultbox/internal/pathmatch"
)

// watch() — file-scoped I/O observation (RFC-054 M5).
//
// Backed by gVisor's seccheck trace points, which report the Sentry-resolved
// path plus byte offset, count and real errno. Observation only: the protocol
// has no return path, so watch() can never deny or delay an operation.

// watchableOps are the operations gVisor actually traces.
//
// fsync is deliberately absent. gVisor traces 42 syscalls and
// fsync/fdatasync/msync/sync_file_range are not among them (M0.3), so
// watch(ops=["fsync"]) is rejected at spec load rather than accepted and
// silently emitting nothing — which would let a durability audit "pass"
// having observed no fsync because none could ever be reported.
var watchableOps = []string{"open", "read", "write", "close", "connect"}

func isWatchableOp(op string) bool {
	for _, o := range watchableOps {
		if o == op {
			return true
		}
	}
	return false
}

// untraceableOps are operations users will reasonably ask for and gVisor
// cannot provide. Named individually so the error explains the gap instead of
// just listing what is allowed.
var untraceableOps = map[string]string{
	"fsync":           "gVisor has no fsync trace point",
	"fdatasync":       "gVisor has no fdatasync trace point",
	"msync":           "gVisor has no msync trace point",
	"sync_file_range": "gVisor has no sync_file_range trace point",
	"sync":            "gVisor has no sync trace point",
}

// watchSpec is one installed watch window.
type watchSpec struct {
	Service string
	Files   []string
	Ops     map[string]bool
}

// matches reports whether an observed operation belongs to this watch.
//
// Matching keys on the resolved path that read/write/close carry. openat's
// pathname is the raw syscall argument and often relative, so it is matched
// only after the sink has assembled it (M0.3).
func (w *watchSpec) matches(io seccheck.FileIO) bool {
	if len(w.Ops) > 0 && !w.Ops[string(io.Op)] {
		return false
	}
	return pathmatch.MatchAny(w.Files, io.Path)
}

// watchRegistry tracks active watch windows per service.
type watchRegistry struct {
	mu      sync.Mutex
	watches map[string][]*watchSpec
	// observed counts operations that matched some watch, so a window that
	// saw nothing can say so rather than silently pass.
	observed map[string]int
}

func newWatchRegistry() *watchRegistry {
	return &watchRegistry{
		watches:  make(map[string][]*watchSpec),
		observed: make(map[string]int),
	}
}

func (r *watchRegistry) add(w *watchSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watches[w.Service] = append(r.watches[w.Service], w)
}

func (r *watchRegistry) remove(w *watchSpec) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.watches[w.Service]
	for i, candidate := range list {
		if candidate == w {
			r.watches[w.Service] = append(list[:i], list[i+1:]...)
			break
		}
	}
	n := r.observed[w.Service]
	return n
}

// match returns the watches that want this operation.
func (r *watchRegistry) match(service string, io seccheck.FileIO) []*watchSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*watchSpec
	for _, w := range r.watches[service] {
		if w.matches(io) {
			out = append(out, w)
		}
	}
	if len(out) > 0 {
		r.observed[service]++
	}
	return out
}

func (r *watchRegistry) active(service string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watches[service]) > 0
}

// parseWatchArgs is shared by watch() and watch_start().
func parseWatchArgs(builtin string, args starlark.Tuple, kwargs []starlark.Tuple) (svc *ServiceDef, spec *watchSpec, run starlark.Callable, err error) {
	if len(args) < 1 {
		return nil, nil, nil, fmt.Errorf("%s() requires a service as its first argument", builtin)
	}
	s, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%s() first argument must be a service, got %s", builtin, args[0].Type())
	}
	if len(args) > 1 {
		return nil, nil, nil, fmt.Errorf("%s() takes one positional argument (the service); pass files=/ops=/run= as keywords", builtin)
	}

	w := &watchSpec{Service: s.Name, Ops: make(map[string]bool)}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "files":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s() files= must be a list of path globs, got %s", builtin, kv[1].Type())
			}
			iter := list.Iterate()
			var item starlark.Value
			for iter.Next(&item) {
				pattern, ok := starlark.AsString(item)
				if !ok {
					iter.Done()
					return nil, nil, nil, fmt.Errorf("%s() files= items must be strings, got %s", builtin, item.Type())
				}
				w.Files = append(w.Files, pattern)
			}
			iter.Done()
		case "ops":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s() ops= must be a list of operation names, got %s", builtin, kv[1].Type())
			}
			iter := list.Iterate()
			var item starlark.Value
			for iter.Next(&item) {
				op, ok := starlark.AsString(item)
				if !ok {
					iter.Done()
					return nil, nil, nil, fmt.Errorf("%s() ops= items must be strings, got %s", builtin, item.Type())
				}
				if reason, untraceable := untraceableOps[op]; untraceable {
					iter.Done()
					return nil, nil, nil, fmt.Errorf(
						"%s() ops=[%q]: %s, so this would observe nothing. "+
							"Watch %q instead to see the data reach the file, and note that write ordering "+
							"is not the same as durability (RFC-054 decision record M0.3)",
						builtin, op, reason, "write")
				}
				if !isWatchableOp(op) {
					iter.Done()
					return nil, nil, nil, fmt.Errorf("%s() ops=[%q]: unknown operation; valid: %s",
						builtin, op, strings.Join(watchableOps, ", "))
				}
				w.Ops[op] = true
			}
			iter.Done()
		case "run":
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s() run= must be a callable, got %s", builtin, kv[1].Type())
			}
			run = cb
		default:
			return nil, nil, nil, fmt.Errorf("%s(): unknown keyword argument %q; valid: files, ops, run", builtin, key)
		}
	}
	return s, w, run, nil
}

// builtinWatch implements watch(service, files=, ops=, run=).
func (rt *Runtime) builtinWatch(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	svc, spec, run, err := parseWatchArgs("watch", args, kwargs)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("watch() requires run= with a callback; use watch_start()/watch_stop() for imperative control")
	}
	if err := rt.requireFileObservation("watch()"); err != nil {
		return nil, err
	}

	rt.watches.add(spec)
	rt.markWatchRan()
	rt.events.Emit("watch_started", svc.Name, map[string]string{
		"files": strings.Join(spec.Files, ","),
		"ops":   joinOps(spec.Ops),
	})
	defer func() {
		n := rt.watches.remove(spec)
		rt.events.Emit("watch_stopped", svc.Name, map[string]string{
			"observed": fmt.Sprintf("%d", n),
		})
	}()

	result, err := starlark.Call(thread, run, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return starlark.None, nil
	}
	return result, nil
}

// builtinWatchStart implements watch_start(service, files=, ops=).
func (rt *Runtime) builtinWatchStart(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	svc, spec, run, err := parseWatchArgs("watch_start", args, kwargs)
	if err != nil {
		return nil, err
	}
	if run != nil {
		return nil, fmt.Errorf("watch_start() does not take run=; use watch() for a scoped window")
	}
	if err := rt.requireFileObservation("watch_start()"); err != nil {
		return nil, err
	}
	rt.watches.add(spec)
	rt.markWatchRan()
	rt.events.Emit("watch_started", svc.Name, map[string]string{
		"files": strings.Join(spec.Files, ","),
		"ops":   joinOps(spec.Ops),
	})
	return starlark.None, nil
}

// markWatchRan records that a watch window opened during this test, so the
// runtime can fail the test if nothing was ever observed.
func (rt *Runtime) markWatchRan() {
	rt.mu.Lock()
	rt.watchRan = true
	rt.mu.Unlock()
}

// builtinWatchStop implements watch_stop(service).
func (rt *Runtime) builtinWatchStop(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("watch_stop() takes exactly one argument (the service)")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("watch_stop() argument must be a service, got %s", args[0].Type())
	}
	rt.watches.mu.Lock()
	list := rt.watches.watches[svc.Name]
	rt.watches.watches[svc.Name] = nil
	n := rt.watches.observed[svc.Name]
	rt.watches.mu.Unlock()

	if len(list) == 0 {
		return nil, fmt.Errorf("watch_stop(%q): no watch is active on this service", svc.Name)
	}
	rt.events.Emit("watch_stopped", svc.Name, map[string]string{
		"observed": fmt.Sprintf("%d", n),
	})
	return starlark.None, nil
}

// requireFileObservation rejects watch() in v0.14.0.
//
// The sink, decoder and DSL are complete and tested, but the mechanism that
// installs a trace session cannot see the traffic that matters.
// `runsc trace create` instruments only tasks created *after* the session
// starts, and Faultbox attaches it once the service is up and healthchecked —
// by which point every worker thread already exists. Measured in the Lima VM:
// a network-driven workload against a running Postgres backend produced 2
// trace points, while the same work driven by a newly-spawned process produced
// 1054 (RFC-054 decision record M5).
//
// So watch() would load, run, observe almost nothing, and let an I/O-surface
// audit pass having verified nothing — the exact failure mode this release was
// built to eliminate. Shipping it with a caveat in the docs would be worse
// than not shipping it.
//
// The fix is runsc's -pod-init-config, which installs trace sessions at
// sandbox boot so every task is instrumented. A 2026-07-29 spike confirmed it
// works: 236 trace points on the same network-driven query that yielded 2, and
// 11,295 before any query at all.
//
// What is left is not tracing. -pod-init-config is a runtime-level flag in
// daemon.json, so the sink path becomes host-wide state that concurrent runs
// collide on — the same class of problem as the shared faultbox0 TUN name
// v0.14.1 fixed, one layer up. That is specified in RFC-056, target v0.15.0.
func (rt *Runtime) requireFileObservation(what string) error {
	return fmt.Errorf(
		"%s is not available yet. gVisor's trace sessions only instrument tasks created "+
			"after the session starts, and Faultbox attaches one after the service is healthy — so a "+
			"watch would observe almost nothing and its assertions would be vacuous. "+
			"The tracing fix is proven (runsc -pod-init-config); what remains is that the flag is "+
			"host-wide daemon.json state. Tracked as RFC-056, target v0.15.0. "+
			"Packet faults (packet_drop, packet_delay, ...) are unaffected and work today",
		what)
}

// onFileIO routes one observed operation to any watch that wants it.
func (rt *Runtime) onFileIO(service string, io seccheck.FileIO) {
	matched := rt.watches.match(service, io)
	if len(matched) == 0 {
		return
	}
	fields := map[string]string{
		"op":     string(io.Op),
		"path":   io.Path,
		"fd":     fmt.Sprintf("%d", io.FD),
		"result": fmt.Sprintf("%d", io.Result),
		"pid":    fmt.Sprintf("%d", io.PID),
	}
	if io.Count > 0 {
		fields["count"] = fmt.Sprintf("%d", io.Count)
	}
	if io.HasOffset {
		fields["offset"] = fmt.Sprintf("%d", io.Offset)
	}
	if io.Errno != 0 {
		fields["errno"] = fmt.Sprintf("%d", io.Errno)
	}
	if io.ProcessName != "" {
		fields["process"] = io.ProcessName
	}
	if io.ShortTransfer() {
		// Surfaced as a field rather than left for the reader to compute:
		// a short write is the precondition for a torn record, and it is
		// easy to miss when count and result are just two numbers.
		fields["short"] = "true"
	}
	rt.events.Emit("file_io", service, fields)
}

func joinOps(ops map[string]bool) string {
	if len(ops) == 0 {
		return "all"
	}
	out := make([]string, 0, len(ops))
	for op := range ops {
		out = append(out, op)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
