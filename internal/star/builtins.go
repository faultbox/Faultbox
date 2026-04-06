package star

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/engine"
)

// parallelResult captures the outcome of one parallel callable.
type parallelResult struct {
	value starlark.Value
	err   error
}

// builtins returns all Starlark built-in functions for a runtime.
func (rt *Runtime) builtins() starlark.StringDict {
	return starlark.StringDict{
		"service":     starlark.NewBuiltin("service", rt.builtinService),
		"interface":   starlark.NewBuiltin("interface", builtinInterface),
		"tcp":         starlark.NewBuiltin("tcp", builtinTCP),
		"http":        starlark.NewBuiltin("http", builtinHTTP),
		"delay":       starlark.NewBuiltin("delay", builtinDelay),
		"deny":        starlark.NewBuiltin("deny", builtinDeny),
		"allow":       starlark.NewBuiltin("allow", builtinAllow),
		"fault":       starlark.NewBuiltin("fault", rt.builtinFault),
		"fault_start": starlark.NewBuiltin("fault_start", rt.builtinFaultStart),
		"fault_stop":  starlark.NewBuiltin("fault_stop", rt.builtinFaultStop),
		"assert_true":       starlark.NewBuiltin("assert_true", builtinAssertTrue),
		"assert_eq":         starlark.NewBuiltin("assert_eq", builtinAssertEq),
		"assert_eventually": starlark.NewBuiltin("assert_eventually", rt.builtinAssertEventually),
		"assert_never":      starlark.NewBuiltin("assert_never", rt.builtinAssertNever),
		"assert_before":     starlark.NewBuiltin("assert_before", rt.builtinAssertBefore),
		"events":            starlark.NewBuiltin("events", rt.builtinEvents),
		"parallel":          starlark.NewBuiltin("parallel", rt.builtinParallel),
		"monitor":           starlark.NewBuiltin("monitor", rt.builtinMonitor),
		"partition":         starlark.NewBuiltin("partition", rt.builtinPartition),
		"nondet":            starlark.NewBuiltin("nondet", rt.builtinNondet),
		"trace":             starlark.NewBuiltin("trace", rt.builtinTrace),
		"trace_start":       starlark.NewBuiltin("trace_start", rt.builtinTraceStart),
		"trace_stop":        starlark.NewBuiltin("trace_stop", rt.builtinTraceStop),
		"scenario":          starlark.NewBuiltin("scenario", rt.builtinScenario),
		"op":                starlark.NewBuiltin("op", builtinOp),
		"stdout":            starlark.NewBuiltin("stdout", builtinStdoutSource),
		"json_decoder":      starlark.NewBuiltin("json_decoder", builtinJSONDecoder),
		"logfmt_decoder":    starlark.NewBuiltin("logfmt_decoder", builtinLogfmtDecoder),
		"regex_decoder":     starlark.NewBuiltin("regex_decoder", builtinRegexDecoder),
	}
}

// service(name, [binary], *interfaces, image=, build=, healthcheck=, env=, depends_on=, volumes=)
// Binary can be positional (2nd arg) or keyword. For containers, use image= or build= instead.
func (rt *Runtime) builtinService(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("service() requires at least a name")
	}

	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("service() name must be a string")
	}

	svc := &ServiceDef{
		Name:       name,
		Interfaces: make(map[string]*InterfaceDef),
		Env:        make(map[string]string),
	}

	// Positional args after name: binary (string) or interfaces.
	for i := 1; i < len(args); i++ {
		switch v := args[i].(type) {
		case starlark.String:
			// Second positional string = binary path.
			svc.Binary = string(v)
		case *InterfaceDef:
			svc.Interfaces[v.Name] = v
		default:
			return nil, fmt.Errorf("service() positional arg %d must be a string (binary) or interface(), got %s", i, args[i].Type())
		}
	}

	// Keyword args.
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "binary":
			s, _ := starlark.AsString(kv[1])
			svc.Binary = s
		case "image":
			s, _ := starlark.AsString(kv[1])
			svc.Image = s
		case "build":
			s, _ := starlark.AsString(kv[1])
			svc.Build = s
		case "healthcheck":
			hc, ok := kv[1].(*HealthcheckDef)
			if !ok {
				return nil, fmt.Errorf("service() healthcheck must be a tcp() or http() value")
			}
			svc.Healthcheck = hc
		case "env":
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("service() env must be a dict")
			}
			for _, item := range dict.Items() {
				k, _ := starlark.AsString(item[0])
				v, _ := starlark.AsString(item[1])
				svc.Env[k] = v
			}
		case "args":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("service() args must be a list")
			}
			iter := list.Iterate()
			var val starlark.Value
			for iter.Next(&val) {
				s, ok := starlark.AsString(val)
				if !ok {
					iter.Done()
					return nil, fmt.Errorf("service() args items must be strings, got %s", val.Type())
				}
				svc.Args = append(svc.Args, s)
			}
			iter.Done()
		case "depends_on":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("service() depends_on must be a list")
			}
			iter := list.Iterate()
			defer iter.Done()
			var val starlark.Value
			for iter.Next(&val) {
				switch dep := val.(type) {
				case *ServiceDef:
					svc.DependsOn = append(svc.DependsOn, dep.Name)
				case starlark.String:
					svc.DependsOn = append(svc.DependsOn, string(dep))
				default:
					return nil, fmt.Errorf("depends_on items must be services or strings, got %s", val.Type())
				}
			}
		case "volumes":
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("service() volumes must be a dict")
			}
			svc.Volumes = make(map[string]string)
			for _, item := range dict.Items() {
				k, _ := starlark.AsString(item[0])
				v, _ := starlark.AsString(item[1])
				svc.Volumes[k] = v
			}
		case "observe":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("service() observe must be a list")
			}
			iter := list.Iterate()
			var val starlark.Value
			for iter.Next(&val) {
				osv, ok := val.(*ObserveSourceVal)
				if !ok {
					iter.Done()
					return nil, fmt.Errorf("service() observe items must be stdout()/topic()/etc, got %s", val.Type())
				}
				svc.Observe = append(svc.Observe, osv.Config)
			}
			iter.Done()
		case "ops":
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("service() ops must be a dict")
			}
			svc.Ops = make(map[string]*OpDef)
			for _, item := range dict.Items() {
				name, _ := starlark.AsString(item[0])
				opDef, ok := item[1].(*OpDef)
				if !ok {
					return nil, fmt.Errorf("service() ops values must be op(), got %s", item[1].Type())
				}
				opDef.Name = name
				svc.Ops[name] = opDef
			}
		}
	}

	// Validate: exactly one of binary, image, or build must be set.
	sources := 0
	if svc.Binary != "" {
		sources++
	}
	if svc.Image != "" {
		sources++
	}
	if svc.Build != "" {
		sources++
	}
	if sources == 0 {
		return nil, fmt.Errorf("service() requires one of: binary (positional or keyword), image=, or build=")
	}
	if sources > 1 {
		return nil, fmt.Errorf("service() accepts only one of: binary, image=, or build= (got %d)", sources)
	}

	rt.registerService(svc)
	return svc, nil
}

// interface(name, protocol, port, spec=)
func builtinInterface(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, protocol string
	var port int
	if err := starlark.UnpackPositionalArgs("interface", args, kwargs, 3, &name, &protocol, &port); err != nil {
		// Try with kwargs for spec.
		if err := starlark.UnpackArgs("interface", args, kwargs, "name", &name, "protocol", &protocol, "port", &port); err != nil {
			return nil, err
		}
	}

	iface := &InterfaceDef{
		Name:     name,
		Protocol: protocol,
		Port:     port,
	}

	if spec, ok := starKwarg(kwargs, "spec"); ok {
		s, _ := starlark.AsString(spec)
		iface.Spec = s
	}

	return iface, nil
}

// tcp(addr, timeout=)
func builtinTCP(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var addr string
	if err := starlark.UnpackPositionalArgs("tcp", args, nil, 1, &addr); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(addr, "tcp://") {
		addr = "tcp://" + addr
	}
	timeout := 10 * time.Second
	if ts, ok := starKwarg(kwargs, "timeout"); ok {
		s, _ := starlark.AsString(ts)
		d, err := parseStarDuration(s)
		if err != nil {
			return nil, fmt.Errorf("tcp() bad timeout: %w", err)
		}
		timeout = d
	}
	return &HealthcheckDef{Test: addr, Timeout: timeout}, nil
}

// http(url, timeout=)
func builtinHTTP(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var url string
	if err := starlark.UnpackPositionalArgs("http", args, nil, 1, &url); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	timeout := 10 * time.Second
	if ts, ok := starKwarg(kwargs, "timeout"); ok {
		s, _ := starlark.AsString(ts)
		d, err := parseStarDuration(s)
		if err != nil {
			return nil, fmt.Errorf("http() bad timeout: %w", err)
		}
		timeout = d
	}
	return &HealthcheckDef{Test: url, Timeout: timeout}, nil
}

// delay(duration, probability=, label=)
func builtinDelay(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var durStr string
	if err := starlark.UnpackPositionalArgs("delay", args, nil, 1, &durStr); err != nil {
		return nil, err
	}
	dur, err := parseStarDuration(durStr)
	if err != nil {
		return nil, fmt.Errorf("delay() bad duration %q: %w", durStr, err)
	}
	prob := 1.0
	if ps, ok := starKwarg(kwargs, "probability"); ok {
		s, _ := starlark.AsString(ps)
		s = strings.TrimSuffix(s, "%")
		fmt.Sscanf(s, "%f", &prob)
		if prob > 1 {
			prob /= 100.0
		}
	}
	var label string
	if ls, ok := starKwarg(kwargs, "label"); ok {
		label, _ = starlark.AsString(ls)
	}
	return &FaultDef{Action: "delay", Delay: dur, Probability: prob, Label: label}, nil
}

// deny(errno, probability=, label=)
func builtinDeny(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var errno string
	if err := starlark.UnpackPositionalArgs("deny", args, nil, 1, &errno); err != nil {
		return nil, err
	}
	prob := 1.0
	if ps, ok := starKwarg(kwargs, "probability"); ok {
		s, _ := starlark.AsString(ps)
		s = strings.TrimSuffix(s, "%")
		fmt.Sscanf(s, "%f", &prob)
		if prob > 1 {
			prob /= 100.0
		}
	}
	var label string
	if ls, ok := starKwarg(kwargs, "label"); ok {
		label, _ = starlark.AsString(ls)
	}
	return &FaultDef{Action: "deny", Errno: strings.ToUpper(errno), Probability: prob, Label: label}, nil
}

// allow()
func builtinAllow(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return &FaultDef{Action: "allow"}, nil
}

// fault(service, run=body_fn, **syscall_faults)
// Example: fault(db, write=delay("500ms"), run=my_func)
func (rt *Runtime) builtinFault(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("fault() first arg must be a service, got %s", args[0].Type())
	}

	// Extract run= callback and fault specs from kwargs.
	// Keys can be syscall names ("write") or operation names ("persist").
	var bodyFn starlark.Callable
	faults := make(map[string]*FaultDef)

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "run" {
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault() run= must be a callable")
			}
			bodyFn = cb
		} else {
			fd, ok := kv[1].(*FaultDef)
			if !ok {
				return nil, fmt.Errorf("fault() %s= must be a fault (delay/deny), got %s", key, kv[1].Type())
			}
			// Check if key is a named operation on this service.
			if svc.Ops != nil {
				if opDef, isOp := svc.Ops[key]; isOp {
					// Expand operation: add fault for each syscall in the op.
					for _, sc := range opDef.Syscalls {
						opFd := *fd // copy
						opFd.Op = key
						if opDef.Path != "" {
							opFd.PathGlob = opDef.Path
						}
						faults[sc] = &opFd
					}
					continue
				}
			}
			faults[key] = fd
		}
	}

	if bodyFn == nil {
		return nil, fmt.Errorf("fault() requires run= keyword with a callback function")
	}

	// Apply faults, run body, remove faults.
	if err := rt.applyFaults(svc.Name, faults); err != nil {
		return nil, fmt.Errorf("fault(): %w", err)
	}
	defer rt.removeFaults(svc.Name)

	result, err := starlark.Call(thread, bodyFn, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return starlark.None, nil
	}
	return result, nil
}

// fault_start(service, **syscall_faults)
func (rt *Runtime) builtinFaultStart(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault_start() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("fault_start() first arg must be a service, got %s", args[0].Type())
	}

	faults := make(map[string]*FaultDef)
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		fd, ok := kv[1].(*FaultDef)
		if !ok {
			return nil, fmt.Errorf("fault_start() %s= must be a fault, got %s", key, kv[1].Type())
		}
		faults[key] = fd
	}

	if err := rt.applyFaults(svc.Name, faults); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

// fault_stop(service)
func (rt *Runtime) builtinFaultStop(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault_stop() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("fault_stop() first arg must be a service, got %s", args[0].Type())
	}
	rt.removeFaults(svc.Name)
	return starlark.None, nil
}

// assert_true(condition, msg=)
func builtinAssertTrue(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("assert_true() requires a condition")
	}
	if !args[0].Truth() {
		msg := "assertion failed"
		if len(args) > 1 {
			msg, _ = starlark.AsString(args[1])
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return starlark.None, nil
}

// assert_eq(a, b, msg=)
func builtinAssertEq(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("assert_eq() requires two arguments")
	}
	eq, err := starlark.Equal(args[0], args[1])
	if err != nil {
		return nil, err
	}
	if !eq {
		msg := fmt.Sprintf("assert_eq failed: %s != %s", args[0], args[1])
		if len(args) > 2 {
			msg, _ = starlark.AsString(args[2])
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return starlark.None, nil
}

// assert_eventually(service=, syscall=, path=, decision=, where=lambda)
// Checks that at least one event in the current trace matches all given filters.
// Supports where=lambda for complex predicates on structured event data.
func (rt *Runtime) builtinAssertEventually(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	whereFn, whereKwargs := extractWhere(kwargs)
	filters := extractEventFilters(whereKwargs)
	events := rt.events.Events()

	for _, ev := range events {
		if ev.Type != "syscall" && ev.Type != "stdout" && ev.Type != "topic" && ev.Type != "wal" {
			continue
		}
		if whereFn != nil {
			se := &StarlarkEvent{ev: ev}
			result, err := starlark.Call(thread, whereFn, starlark.Tuple{se}, nil)
			if err != nil {
				return nil, fmt.Errorf("assert_eventually: where= callback failed: %w", err)
			}
			if result.Truth() {
				return starlark.None, nil
			}
		} else if matchesFilters(ev, filters) {
			return starlark.None, nil
		}
	}

	if whereFn != nil {
		return nil, fmt.Errorf("assert_eventually: no event matched where= predicate")
	}
	return nil, fmt.Errorf("assert_eventually: no matching event found (filters: %s)", formatFilters(filters))
}

// assert_never(service=, syscall=, path=, decision=, where=lambda)
// Checks that no event in the current trace matches all given filters.
func (rt *Runtime) builtinAssertNever(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	whereFn, whereKwargs := extractWhere(kwargs)
	filters := extractEventFilters(whereKwargs)
	events := rt.events.Events()

	for _, ev := range events {
		if ev.Type != "syscall" && ev.Type != "stdout" && ev.Type != "topic" && ev.Type != "wal" {
			continue
		}
		if whereFn != nil {
			se := &StarlarkEvent{ev: ev}
			result, err := starlark.Call(thread, whereFn, starlark.Tuple{se}, nil)
			if err != nil {
				return nil, fmt.Errorf("assert_never: where= callback failed: %w", err)
			}
			if result.Truth() {
				return nil, fmt.Errorf("assert_never: found matching event #%d via where= predicate", ev.Seq)
			}
		} else if matchesFilters(ev, filters) {
			return nil, fmt.Errorf("assert_never: found matching event #%d (service=%s syscall=%s decision=%s path=%s)",
				ev.Seq, ev.Service, ev.Fields["syscall"], ev.Fields["decision"], ev.Fields["path"])
		}
	}

	return starlark.None, nil
}

// eventFilter holds a single filter criterion.
type eventFilter struct {
	key   string // "service", "syscall", "path", "decision"
	value string // value to match (supports trailing * for glob)
}

// extractWhere separates the where= kwarg from other kwargs.
// Returns the where callable (or nil) and the remaining kwargs.
func extractWhere(kwargs []starlark.Tuple) (starlark.Callable, []starlark.Tuple) {
	var whereFn starlark.Callable
	var rest []starlark.Tuple
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "where" {
			if cb, ok := kv[1].(starlark.Callable); ok {
				whereFn = cb
			}
		} else {
			rest = append(rest, kv)
		}
	}
	return whereFn, rest
}

func extractEventFilters(kwargs []starlark.Tuple) []eventFilter {
	var filters []eventFilter
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		val, _ := starlark.AsString(kv[1])
		filters = append(filters, eventFilter{key: key, value: val})
	}
	return filters
}

func matchesFilters(ev Event, filters []eventFilter) bool {
	for _, f := range filters {
		var actual string
		switch f.key {
		case "service":
			actual = ev.Service
		case "syscall":
			actual = ev.Fields["syscall"]
		case "path":
			actual = ev.Fields["path"]
		case "decision":
			actual = ev.Fields["decision"]
		default:
			actual = ev.Fields[f.key]
		}
		if !matchValue(actual, f.value) {
			return false
		}
	}
	return true
}

// matchValue checks if actual matches the pattern.
// Supports trailing * for prefix matching and leading * for suffix matching.
func matchValue(actual, pattern string) bool {
	if pattern == "" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(actual, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(actual, strings.TrimPrefix(pattern, "*"))
	}
	return actual == pattern
}

// trace(service, syscalls=["write", "openat"], run=body_fn)
// Installs seccomp filters in allow-only mode so syscalls are logged without faulting.
func (rt *Runtime) builtinTrace(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("trace() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("trace() first arg must be a service, got %s", args[0].Type())
	}

	var bodyFn starlark.Callable
	var syscalls []string

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "run":
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("trace() run= must be a callable")
			}
			bodyFn = cb
		case "syscalls":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("trace() syscalls= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				s, ok := starlark.AsString(list.Index(i))
				if !ok {
					return nil, fmt.Errorf("trace() syscalls list items must be strings")
				}
				syscalls = append(syscalls, s)
			}
		default:
			return nil, fmt.Errorf("trace() unexpected keyword %q (use syscalls= for syscall list)", key)
		}
	}

	if bodyFn == nil {
		return nil, fmt.Errorf("trace() requires run= keyword with a callback function")
	}
	if len(syscalls) == 0 {
		return nil, fmt.Errorf("trace() requires syscalls= keyword with a list of syscall names")
	}

	if err := rt.applyTrace(svc.Name, syscalls); err != nil {
		return nil, fmt.Errorf("trace(): %w", err)
	}
	defer rt.removeTrace(svc.Name)

	result, err := starlark.Call(thread, bodyFn, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return starlark.None, nil
	}
	return result, nil
}

// trace_start(service, syscalls=["write", "openat"])
func (rt *Runtime) builtinTraceStart(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("trace_start() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("trace_start() first arg must be a service, got %s", args[0].Type())
	}

	var syscalls []string
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "syscalls" {
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("trace_start() syscalls= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				s, ok := starlark.AsString(list.Index(i))
				if !ok {
					return nil, fmt.Errorf("trace_start() syscalls list items must be strings")
				}
				syscalls = append(syscalls, s)
			}
		}
	}

	if len(syscalls) == 0 {
		return nil, fmt.Errorf("trace_start() requires syscalls= keyword with a list of syscall names")
	}

	if err := rt.applyTrace(svc.Name, syscalls); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

// trace_stop(service)
func (rt *Runtime) builtinTraceStop(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("trace_stop() requires a service argument")
	}
	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("trace_stop() first arg must be a service, got %s", args[0].Type())
	}
	rt.removeTrace(svc.Name)
	return starlark.None, nil
}

// scenario(fn) — registers a happy-path function for the failure generator.
// The function is also run as a test (equivalent to test_<name>).
func (rt *Runtime) builtinScenario(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("scenario() requires exactly one callable")
	}
	callable, ok := args[0].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("scenario() argument must be a callable, got %s", args[0].Type())
	}

	rt.scenarios = append(rt.scenarios, ScenarioRegistration{
		Name: callable.Name(),
		Fn:   callable,
	})

	return starlark.None, nil
}

// nondet(service, ...) — marks one or more services as nondeterministic,
// excluding them from interleaving control during parallel().
// Their syscalls proceed immediately.
func (rt *Runtime) builtinNondet(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("nondet() requires at least one service argument")
	}
	if rt.nondetServices == nil {
		rt.nondetServices = make(map[string]bool)
	}
	for i, arg := range args {
		svc, ok := arg.(*ServiceDef)
		if !ok {
			return nil, fmt.Errorf("nondet() argument %d must be a service, got %s", i, arg.Type())
		}
		rt.nondetServices[svc.Name] = true
	}
	return starlark.None, nil
}

// parallel(fn1, fn2, ...) → list of results
// Runs multiple step callables concurrently. Each callable runs in its own
// goroutine with a dedicated Starlark thread. Returns results in the same
// order as the arguments.
//
// When explore mode is active (--explore=all or --explore=sample), parallel()
// installs hold rules to control syscall release ordering. The ExploreScheduler
// uses the current permutation index to determine which held syscall to release
// next, enabling deterministic interleaving exploration.
func (rt *Runtime) builtinParallel(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("parallel() requires at least 2 callables")
	}

	// Verify all args are callable.
	callables := make([]starlark.Callable, len(args))
	for i, arg := range args {
		c, ok := arg.(starlark.Callable)
		if !ok {
			return nil, fmt.Errorf("parallel() argument %d is not callable (got %s)", i, arg.Type())
		}
		callables[i] = c
	}

	// If explore mode is active, install hold rules and use scheduler.
	if rt.exploreMode == "all" || rt.exploreMode == "sample" {
		return rt.parallelWithExplore(callables)
	}

	return rt.parallelSimple(callables)
}

// parallelSimple runs callables concurrently without interleaving control.
func (rt *Runtime) parallelSimple(callables []starlark.Callable) (starlark.Value, error) {
	results := make([]parallelResult, len(callables))
	var wg sync.WaitGroup

	for i, c := range callables {
		wg.Add(1)
		go func(idx int, callable starlark.Callable) {
			defer wg.Done()
			t := &starlark.Thread{Name: fmt.Sprintf("parallel-%d", idx)}
			val, err := starlark.Call(t, callable, nil, nil)
			results[idx] = parallelResult{value: val, err: err}
		}(i, c)
	}

	wg.Wait()
	return rt.collectParallelResults(results)
}

// parallelWithExplore runs callables with hold-and-release scheduling.
// Syscalls from non-nondet services are held and released in permutation order.
func (rt *Runtime) parallelWithExplore(callables []starlark.Callable) (starlark.Value, error) {
	holdTag := fmt.Sprintf("explore-%d", rt.explorePerm)

	// Install hold rules on all non-nondet services.
	for svcName, rs := range rt.sessions {
		if rt.nondetServices[svcName] {
			continue
		}
		// Hold all pre-installed syscalls.
		var rules []engine.FaultRule
		for _, sc := range []string{"write", "read", "connect", "openat", "fsync", "sendto", "recvfrom", "writev"} {
			rules = append(rules, engine.FaultRule{
				Syscall:     sc,
				Action:      engine.ActionHold,
				Probability: 1.0,
			})
		}
		rs.session.RegisterHoldQueue(holdTag)
		rs.session.AddHoldRules(holdTag, rules)
	}

	// Cleanup hold rules when done.
	defer func() {
		for svcName, rs := range rt.sessions {
			if rt.nondetServices[svcName] {
				continue
			}
			rs.session.RemoveHoldRules(holdTag)
		}
	}()

	// Launch callables concurrently.
	results := make([]parallelResult, len(callables))
	var wg sync.WaitGroup

	for i, c := range callables {
		wg.Add(1)
		go func(idx int, callable starlark.Callable) {
			defer wg.Done()
			t := &starlark.Thread{Name: fmt.Sprintf("parallel-%d", idx)}
			val, err := starlark.Call(t, callable, nil, nil)
			results[idx] = parallelResult{value: val, err: err}
		}(i, c)
	}

	// Run the scheduler: collect held syscalls and release in permutation order.
	// We do this in a goroutine so callables can proceed as releases happen.
	schedDone := make(chan error, 1)
	go func() {
		// Collect held syscalls from all non-nondet services into a combined queue.
		// For simplicity, use the first non-nondet service's queue.
		// TODO: merge queues from multiple services for multi-service parallel.
		var q *engine.HoldQueue
		for svcName, rs := range rt.sessions {
			if rt.nondetServices[svcName] {
				continue
			}
			q = rs.session.GetHoldQueue(holdTag)
			if q != nil {
				break
			}
		}
		if q == nil {
			schedDone <- nil
			return
		}

		// Wait a bit for syscalls to arrive, then release in permutation order.
		// The scheduler releases all held syscalls using the permutation.
		scheduler := &engine.ExploreScheduler{PermIndex: rt.explorePerm}

		// Wait for at least 1 held syscall, with timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := q.Wait(1, ctx); err != nil {
			// No syscalls held — callables may have completed without hitting holds.
			schedDone <- nil
			return
		}

		// Give a moment for more syscalls to accumulate.
		time.Sleep(50 * time.Millisecond)

		n := q.Len()
		if n > 0 {
			// Record held count for auto-permutation calculation.
			rt.exploreHeldN = n
			_, err := scheduler.ReleaseInOrder(ctx, q, n)
			schedDone <- err
		} else {
			schedDone <- nil
		}
	}()

	wg.Wait()
	<-schedDone

	return rt.collectParallelResults(results)
}

func (rt *Runtime) collectParallelResults(results []parallelResult) (starlark.Value, error) {
	resultList := make([]starlark.Value, len(results))
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("parallel() callable %d failed: %w", i, r.err)
		}
		if r.value == nil {
			resultList[i] = starlark.None
		} else {
			resultList[i] = r.value
		}
	}
	return starlark.NewList(resultList), nil
}

// monitor(callback, service=, syscall=, path=, decision=)
// Registers a continuous monitor that is called on every matching event.
// The callback receives an event dict. If the callback raises an error,
// the test fails with "monitor violation".
func (rt *Runtime) builtinMonitor(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("monitor() requires a callback")
	}
	callback, ok := args[0].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("monitor() first argument must be callable, got %s", args[0].Type())
	}

	// Remaining kwargs are event filters.
	var filters []eventFilter
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		val, _ := starlark.AsString(kv[1])
		filters = append(filters, eventFilter{key: key, value: val})
	}

	// Subscribe to matching events.
	rt.events.Subscribe(filters, func(ev Event) error {
		// Build event dict for the Starlark callback.
		d := starlark.NewDict(6)
		d.SetKey(starlark.String("seq"), starlark.MakeInt64(ev.Seq))
		d.SetKey(starlark.String("type"), starlark.String(ev.Type))
		d.SetKey(starlark.String("service"), starlark.String(ev.Service))
		for k, v := range ev.Fields {
			d.SetKey(starlark.String(k), starlark.String(v))
		}

		// Call Starlark callback on a fresh thread (safe from goroutines).
		t := &starlark.Thread{Name: "monitor"}
		_, err := starlark.Call(t, callback, starlark.Tuple{d}, nil)
		if err != nil {
			rt.monitorMu.Lock()
			rt.monitorErrors = append(rt.monitorErrors, err)
			rt.monitorMu.Unlock()
			return err
		}
		return nil
	})

	return starlark.None, nil
}

// partition(svc_a, svc_b, run=callback)
// Creates a network partition between two services. While the callback runs,
// svc_a cannot connect to svc_b and svc_b cannot connect to svc_a.
func (rt *Runtime) builtinPartition(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("partition() requires two service arguments")
	}
	svcA, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("partition() first arg must be a service, got %s", args[0].Type())
	}
	svcB, ok := args[1].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("partition() second arg must be a service, got %s", args[1].Type())
	}

	var bodyFn starlark.Callable
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "run" {
			cb, cbOk := kv[1].(starlark.Callable)
			if !cbOk {
				return nil, fmt.Errorf("partition() run= must be a callable")
			}
			bodyFn = cb
		}
	}
	if bodyFn == nil {
		return nil, fmt.Errorf("partition() requires run= keyword with a callback function")
	}

	// Resolve destination addresses from service interfaces.
	// svc_a blocks connect to all of svc_b's interface ports, and vice versa.
	var rulesA, rulesB []engine.FaultRule
	for _, iface := range svcB.Interfaces {
		rulesA = append(rulesA, engine.FaultRule{
			Syscall:     "connect",
			Action:      engine.ActionDeny,
			Errno:       111, // ECONNREFUSED
			Probability: 1.0,
			DestAddr:    fmt.Sprintf("127.0.0.1:%d", iface.Port),
		})
	}
	for _, iface := range svcA.Interfaces {
		rulesB = append(rulesB, engine.FaultRule{
			Syscall:     "connect",
			Action:      engine.ActionDeny,
			Errno:       111, // ECONNREFUSED
			Probability: 1.0,
			DestAddr:    fmt.Sprintf("127.0.0.1:%d", iface.Port),
		})
	}

	// Apply partition rules.
	rsA, okA := rt.sessions[svcA.Name]
	rsB, okB := rt.sessions[svcB.Name]
	if !okA {
		return nil, fmt.Errorf("partition(): service %q is not running", svcA.Name)
	}
	if !okB {
		return nil, fmt.Errorf("partition(): service %q is not running", svcB.Name)
	}
	rsA.session.SetDynamicFaultRules(rulesA)
	rsB.session.SetDynamicFaultRules(rulesB)
	rt.events.Emit("partition_applied", "", map[string]string{
		"between": svcA.Name + "," + svcB.Name,
	})

	// Run body, then remove partition.
	defer func() {
		rsA.session.ClearDynamicFaultRules()
		rsB.session.ClearDynamicFaultRules()
		rt.events.Emit("partition_removed", "", map[string]string{
			"between": svcA.Name + "," + svcB.Name,
		})
	}()

	result, err := starlark.Call(thread, bodyFn, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return starlark.None, nil
	}
	return result, nil
}

// assert_before(first={filters}, then={filters})
// Asserts that the first matching event for "first" occurs before the first matching event for "then".
func (rt *Runtime) builtinAssertBefore(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var firstDict, thenDict *starlark.Dict
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "first":
			d, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("assert_before: first must be a dict")
			}
			firstDict = d
		case "then":
			d, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("assert_before: then must be a dict")
			}
			thenDict = d
		}
	}
	if firstDict == nil || thenDict == nil {
		return nil, fmt.Errorf("assert_before: requires first={...} and then={...} keyword arguments")
	}

	firstFilters := dictToFilters(firstDict)
	thenFilters := dictToFilters(thenDict)

	events := rt.events.Events()

	firstSeq := int64(-1)
	thenSeq := int64(-1)

	for _, ev := range events {
		if ev.Type != "syscall" {
			continue
		}
		if firstSeq < 0 && matchesFilters(ev, firstFilters) {
			firstSeq = ev.Seq
		}
		if thenSeq < 0 && matchesFilters(ev, thenFilters) {
			thenSeq = ev.Seq
		}
		if firstSeq >= 0 && thenSeq >= 0 {
			break
		}
	}

	if firstSeq < 0 {
		return nil, fmt.Errorf("assert_before: no event matching 'first' filters (%s)", formatFilters(firstFilters))
	}
	if thenSeq < 0 {
		return nil, fmt.Errorf("assert_before: no event matching 'then' filters (%s)", formatFilters(thenFilters))
	}
	if firstSeq >= thenSeq {
		return nil, fmt.Errorf("assert_before: 'first' event (seq=%d) did not occur before 'then' event (seq=%d)", firstSeq, thenSeq)
	}

	return starlark.None, nil
}

// dictToFilters converts a Starlark dict to eventFilter slice.
func dictToFilters(d *starlark.Dict) []eventFilter {
	var filters []eventFilter
	for _, item := range d.Items() {
		k, _ := starlark.AsString(item[0])
		v, _ := starlark.AsString(item[1])
		filters = append(filters, eventFilter{key: k, value: v})
	}
	return filters
}

// events(service=, syscall=, path=, decision=, where=lambda)
// Returns a list of matching events from the current test's trace.
func (rt *Runtime) builtinEvents(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	whereFn, whereKwargs := extractWhere(kwargs)
	filters := extractEventFilters(whereKwargs)
	events := rt.events.Events()

	var result []starlark.Value
	for _, ev := range events {
		if ev.Type != "syscall" && ev.Type != "stdout" && ev.Type != "topic" && ev.Type != "wal" {
			continue
		}

		se := &StarlarkEvent{ev: ev}

		if whereFn != nil {
			res, err := starlark.Call(thread, whereFn, starlark.Tuple{se}, nil)
			if err != nil {
				return nil, fmt.Errorf("events(): where= callback failed: %w", err)
			}
			if !res.Truth() {
				continue
			}
		} else if len(filters) > 0 && !matchesFilters(ev, filters) {
			continue
		}

		result = append(result, se)
	}

	return starlark.NewList(result), nil
}

func formatFilters(filters []eventFilter) string {
	parts := make([]string, len(filters))
	for i, f := range filters {
		parts[i] = fmt.Sprintf("%s=%q", f.key, f.value)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// EventSource + Decoder builtins for observe=
// ---------------------------------------------------------------------------

// ObserveSourceVal is a Starlark value representing an observe source config.
type ObserveSourceVal struct {
	Config ObserveConfig
}

var _ starlark.Value = (*ObserveSourceVal)(nil)

func (v *ObserveSourceVal) String() string      { return fmt.Sprintf("<observe %s>", v.Config.SourceName) }
func (v *ObserveSourceVal) Type() string         { return "observe_source" }
func (v *ObserveSourceVal) Freeze()              {}
func (v *ObserveSourceVal) Truth() starlark.Bool { return true }
func (v *ObserveSourceVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: observe_source") }

// DecoderVal is a Starlark value representing a decoder config.
type DecoderVal struct {
	Name   string
	Params map[string]string
}

var _ starlark.Value = (*DecoderVal)(nil)

func (v *DecoderVal) String() string      { return fmt.Sprintf("<decoder %s>", v.Name) }
func (v *DecoderVal) Type() string         { return "decoder" }
func (v *DecoderVal) Freeze()              {}
func (v *DecoderVal) Truth() starlark.Bool { return true }
func (v *DecoderVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: decoder") }

// stdout(decoder=) — creates an observe source config for stdout capture.
// op(syscalls=, path=) — defines a named operation (group of syscalls + path filter).
func builtinOp(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	op := &OpDef{}

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "syscalls":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("op() syscalls must be a list")
			}
			iter := list.Iterate()
			var val starlark.Value
			for iter.Next(&val) {
				s, ok := starlark.AsString(val)
				if !ok {
					iter.Done()
					return nil, fmt.Errorf("op() syscalls items must be strings")
				}
				op.Syscalls = append(op.Syscalls, s)
			}
			iter.Done()
		case "path":
			s, _ := starlark.AsString(kv[1])
			op.Path = s
		}
	}

	if len(op.Syscalls) == 0 {
		return nil, fmt.Errorf("op() requires syscalls= argument")
	}

	return op, nil
}

func builtinStdoutSource(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	cfg := ObserveConfig{
		SourceName: "stdout",
		Params:     make(map[string]string),
	}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "decoder" {
			if dv, ok := kv[1].(*DecoderVal); ok {
				cfg.DecoderName = dv.Name
				for k, v := range dv.Params {
					cfg.Params["decoder_"+k] = v
				}
			}
		}
	}
	return &ObserveSourceVal{Config: cfg}, nil
}

// json_decoder() — creates a JSON decoder config.
func builtinJSONDecoder(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return &DecoderVal{Name: "json"}, nil
}

// logfmt_decoder() — creates a logfmt decoder config.
func builtinLogfmtDecoder(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return &DecoderVal{Name: "logfmt"}, nil
}

// regex_decoder(pattern=) — creates a regex decoder config.
func builtinRegexDecoder(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	params := make(map[string]string)
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		val, _ := starlark.AsString(kv[1])
		params[key] = val
	}
	if _, ok := params["pattern"]; !ok {
		return nil, fmt.Errorf("regex_decoder() requires pattern= argument")
	}
	return &DecoderVal{Name: "regex", Params: params}, nil
}
