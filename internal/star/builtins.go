package star

import (
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

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
	}
}

// service(name, binary, *interfaces, healthcheck=, env=, depends_on=, spec=)
func (rt *Runtime) builtinService(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("service() requires at least name and binary")
	}

	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("service() name must be a string")
	}
	binary, ok := starlark.AsString(args[1])
	if !ok {
		return nil, fmt.Errorf("service() binary must be a string")
	}

	svc := &ServiceDef{
		Name:       name,
		Binary:     binary,
		Interfaces: make(map[string]*InterfaceDef),
		Env:        make(map[string]string),
	}

	// Positional args after name and binary are interfaces.
	for i := 2; i < len(args); i++ {
		iface, ok := args[i].(*InterfaceDef)
		if !ok {
			return nil, fmt.Errorf("service() positional arg %d must be an interface(), got %s", i, args[i].Type())
		}
		svc.Interfaces[iface.Name] = iface
	}

	// Keyword args.
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
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
		}
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

// delay(duration, probability=)
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
	return &FaultDef{Action: "delay", Delay: dur, Probability: prob}, nil
}

// deny(errno, probability=)
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
	return &FaultDef{Action: "deny", Errno: strings.ToUpper(errno), Probability: prob}, nil
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

// assert_eventually(service=, syscall=, path=, decision=)
// Checks that at least one event in the current trace matches all given filters.
func (rt *Runtime) builtinAssertEventually(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	filters := extractEventFilters(kwargs)
	events := rt.events.Events()

	for _, ev := range events {
		if ev.Type != "syscall" {
			continue
		}
		if matchesFilters(ev, filters) {
			return starlark.None, nil
		}
	}

	return nil, fmt.Errorf("assert_eventually: no matching event found (filters: %s)", formatFilters(filters))
}

// assert_never(service=, syscall=, path=, decision=)
// Checks that no event in the current trace matches all given filters.
func (rt *Runtime) builtinAssertNever(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	filters := extractEventFilters(kwargs)
	events := rt.events.Events()

	for _, ev := range events {
		if ev.Type != "syscall" {
			continue
		}
		if matchesFilters(ev, filters) {
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

func formatFilters(filters []eventFilter) string {
	parts := make([]string, len(filters))
	for i, f := range filters {
		parts[i] = fmt.Sprintf("%s=%q", f.key, f.value)
	}
	return strings.Join(parts, ", ")
}
