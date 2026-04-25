package star

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/proxy"
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
		"kafka_ready": starlark.NewBuiltin("kafka_ready", builtinKafkaReady),
		"delay":       starlark.NewBuiltin("delay", builtinDelay),
		"deny":        starlark.NewBuiltin("deny", builtinDeny),
		"allow":       starlark.NewBuiltin("allow", builtinAllow),
		"fault":       starlark.NewBuiltin("fault", rt.builtinFault),
		"fault_all":   starlark.NewBuiltin("fault_all", rt.builtinFaultAll),
		"fault_start": starlark.NewBuiltin("fault_start", rt.builtinFaultStart),
		"fault_stop":  starlark.NewBuiltin("fault_stop", rt.builtinFaultStop),
		"assert_true":       starlark.NewBuiltin("assert_true", rt.builtinAssertTrue),
		"assert_eq":         starlark.NewBuiltin("assert_eq", rt.builtinAssertEq),
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
		"fault_assumption":  starlark.NewBuiltin("fault_assumption", rt.builtinFaultAssumption),
		"fault_scenario":    starlark.NewBuiltin("fault_scenario", rt.builtinFaultScenario),
		"fault_matrix":      starlark.NewBuiltin("fault_matrix", rt.builtinFaultMatrix),
		"op":                starlark.NewBuiltin("op", builtinOp),
		"response":          starlark.NewBuiltin("response", builtinProxyResponse),
		"error":             starlark.NewBuiltin("error", builtinProxyError),
		"drop":              starlark.NewBuiltin("drop", builtinProxyDrop),
		"duplicate":         starlark.NewBuiltin("duplicate", builtinProxyDuplicate),
		"stdout":            starlark.NewBuiltin("stdout", builtinStdoutSource),
		"json_decoder":      starlark.NewBuiltin("json_decoder", builtinJSONDecoder),
		"logfmt_decoder":    starlark.NewBuiltin("logfmt_decoder", builtinLogfmtDecoder),
		"regex_decoder":     starlark.NewBuiltin("regex_decoder", builtinRegexDecoder),
		// struct(**kwargs) — namespace objects. Used by recipe modules so
		// protocol-specific helpers don't collide on common names (e.g.
		// mongodb.disk_full vs postgres.disk_full).
		"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
		// Mock services (RFC-017).
		"mock_service":   starlark.NewBuiltin("mock_service", rt.builtinMockService),
		"json_response":  starlark.NewBuiltin("json_response", builtinJSONResponse),
		"text_response":  starlark.NewBuiltin("text_response", builtinTextResponse),
		"bytes_response": starlark.NewBuiltin("bytes_response", builtinBytesResponse),
		"status_only":    starlark.NewBuiltin("status_only", builtinStatusOnly),
		"redirect":       starlark.NewBuiltin("redirect", builtinRedirect),
		"dynamic":        starlark.NewBuiltin("dynamic", builtinDynamic),
		"grpc_response":       starlark.NewBuiltin("grpc_response", builtinGRPCResponse),
		"grpc_typed_response": starlark.NewBuiltin("grpc_typed_response", builtinGRPCTypedResponse),
		"grpc_raw_response":   starlark.NewBuiltin("grpc_raw_response", builtinGRPCRawResponse),
		"grpc_error":          starlark.NewBuiltin("grpc_error", builtinGRPCError),
		// Spec-load-time file readers (RFC-026 / v0.9.8). Paths resolve
		// relative to the spec's directory; network schemes, oversize
		// files, and non-string map keys are refused with clear errors.
		"load_file": starlark.NewBuiltin("load_file", rt.builtinLoadFile),
		"load_yaml": starlark.NewBuiltin("load_yaml", rt.builtinLoadYAML),
		"load_json": starlark.NewBuiltin("load_json", rt.builtinLoadJSON),
		// Fault-matrix outcome predicates (RFC-027 / v0.9.8). Drop into
		// default_expect=/overrides= as replacements for the hand-rolled
		// assertion lambdas every mature spec grows.
		"expect_success":      starlark.NewBuiltin("expect_success", builtinExpectSuccess),
		"expect_error_within": starlark.NewBuiltin("expect_error_within", builtinExpectErrorWithin),
		"expect_hang":         starlark.NewBuiltin("expect_hang", builtinExpectHang),
		// JWT primitives backing @faultbox/mocks/jwt.star (customer ask
		// B3 — v0.9.9). Three thin shims over internal/jwt; users
		// normally reach for jwt.server() in the stdlib, not these
		// builtins directly.
		"jwt_keypair": starlark.NewBuiltin("jwt_keypair", builtinJWTKeypair),
		"jwt_sign":    starlark.NewBuiltin("jwt_sign", builtinJWTSign),
		"jwt_jwks":    starlark.NewBuiltin("jwt_jwks", builtinJWTJWKS),
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
		case "ports":
			// ports = {container_port: host_port} — explicit host port mapping.
			// host_port=0 means Docker picks a random port (default behaviour).
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("service() ports must be a dict {container_port: host_port}")
			}
			svc.Ports = make(map[int]int)
			for _, item := range dict.Items() {
				cp, ok := item[0].(starlark.Int)
				if !ok {
					return nil, fmt.Errorf("service() ports keys must be integers (container ports)")
				}
				hp, ok := item[1].(starlark.Int)
				if !ok {
					return nil, fmt.Errorf("service() ports values must be integers (host ports)")
				}
				cpInt, _ := cp.Int64()
				hpInt, _ := hp.Int64()
				svc.Ports[int(cpInt)] = int(hpInt)
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
		case "reuse":
			b, ok := kv[1].(starlark.Bool)
			if !ok {
				return nil, fmt.Errorf("service() reuse must be a bool")
			}
			svc.Reuse = bool(b)
		case "seed":
			fn, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("service() seed must be a callable (function)")
			}
			svc.Seed = fn
		case "reset":
			fn, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("service() reset must be a callable (function)")
			}
			svc.Reset = fn
		case "seccomp":
			// seccomp=False opts this service out of syscall-level fault
			// injection. Proxy-level faults (HTTP/SQL/Redis/etc.) still apply.
			// Workaround for multi-process entrypoints where shim handoff hangs.
			b, ok := kv[1].(starlark.Bool)
			if !ok {
				return nil, fmt.Errorf("service() seccomp must be a bool (True or False)")
			}
			svc.NoSeccomp = !bool(b)
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

	// Warn about potential state leaks with reuse but no lifecycle handlers.
	if svc.Reuse && svc.Seed == nil && svc.Reset == nil {
		rt.log.Warn("service has reuse=True but no seed or reset — state may leak between tests",
			slog.String("service", svc.Name),
		)
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

// kafka_ready(addr, timeout=) — healthcheck that actually verifies Kafka protocol readiness.
// Unlike tcp(), this check connects with the Kafka protocol, ensuring the broker
// is fully initialised (not just the docker-proxy listening on the host port).
func builtinKafkaReady(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var addr string
	if err := starlark.UnpackPositionalArgs("kafka_ready", args, nil, 1, &addr); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(addr, "kafka://") {
		addr = "kafka://" + addr
	}
	timeout := 120 * time.Second
	if ts, ok := starKwarg(kwargs, "timeout"); ok {
		s, _ := starlark.AsString(ts)
		d, err := parseStarDuration(s)
		if err != nil {
			return nil, fmt.Errorf("kafka_ready() bad timeout: %w", err)
		}
		timeout = d
	}
	return &HealthcheckDef{Test: addr, Timeout: timeout}, nil
}

// delay(duration, probability=, label=)
// Syscall level: delay("500ms") → FaultDef
// Protocol level: delay(path="/data/*", delay="500ms") → ProxyFaultDef
func builtinDelay(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	// Protocol-level delay: no positional args, has delay= kwarg.
	if len(args) == 0 {
		pf := &ProxyFaultDef{Action: "delay"}
		for _, kv := range kwargs {
			key, _ := starlark.AsString(kv[0])
			switch key {
			case "delay":
				s, _ := starlark.AsString(kv[1])
				d, err := parseStarDuration(s)
				if err != nil {
					return nil, fmt.Errorf("delay() bad duration %q: %w", s, err)
				}
				pf.Delay = d
			case "method", "op":
				pf.Method, _ = starlark.AsString(kv[1])
			case "path":
				pf.Path, _ = starlark.AsString(kv[1])
			case "query":
				pf.Query, _ = starlark.AsString(kv[1])
			case "key", "collection":
				pf.Key, _ = starlark.AsString(kv[1])
			case "command":
				pf.Command, _ = starlark.AsString(kv[1])
			case "topic":
				pf.Topic, _ = starlark.AsString(kv[1])
			case "probability":
				pf.Probability = parseProbability(kv[1])
			}
		}
		if pf.Delay == 0 {
			return nil, fmt.Errorf("delay() requires delay= or a positional duration argument")
		}
		return pf, nil
	}

	// Syscall-level delay: positional duration.
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
		prob = parseProbability(ps)
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
		prob = parseProbability(ps)
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

// fault(service_or_interface_ref, ..., run=body_fn)
//
// Syscall level:   fault(db, write=deny("EIO"), run=fn)
// Protocol level:  fault(db.main, response(status=503), run=fn)
func (rt *Runtime) builtinFault(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault() requires a service or interface argument")
	}

	// FaultAssumption as first arg: apply all its rules.
	if assumption, ok := args[0].(*FaultAssumptionDef); ok {
		return rt.builtinFaultFromAssumption(thread, assumption, kwargs)
	}

	// Protocol-level fault: first arg is InterfaceRef.
	if ifRef, ok := args[0].(*InterfaceRef); ok {
		return rt.builtinFaultProtocol(thread, ifRef, args[1:], kwargs)
	}

	svc, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("fault() first arg must be a service, interface_ref, or fault_assumption, got %s", args[0].Type())
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
			// Validate that the key is a known syscall or named operation FIRST,
			// before checking the value type. This gives better errors for
			// unknown keywords like reject=True or latency=delay().
			isOp := svc.Ops != nil && svc.Ops[key] != nil
			if !isOp && !isFaultableSyscall(key) {
				opNames := make([]string, 0)
				if svc.Ops != nil {
					for opName := range svc.Ops {
						opNames = append(opNames, opName)
					}
				}
				hint := fmt.Sprintf("fault() unknown keyword %q for service %q. Valid syscalls: %s",
					key, svc.Name, strings.Join(faultableSyscalls, ", "))
				if len(opNames) > 0 {
					sort.Strings(opNames)
					hint += fmt.Sprintf(". Named operations on %s: %s", svc.Name, strings.Join(opNames, ", "))
				}
				return nil, fmt.Errorf("%s", hint)
			}

			fd, ok := kv[1].(*FaultDef)
			if !ok {
				return nil, fmt.Errorf("fault() %s= must be a fault (delay/deny/allow), got %s. Example: %s=deny(\"ECONNREFUSED\")",
					key, kv[1].Type(), key)
			}
			// Check if key is a named operation on this service.
			if svc.Ops != nil {
				if opDef, isOp := svc.Ops[key]; isOp {
					for _, sc := range opDef.Syscalls {
						opFd := *fd
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

// builtinFaultFromAssumption applies a FaultAssumptionDef's rules, runs the callback, then removes faults.
func (rt *Runtime) builtinFaultFromAssumption(thread *starlark.Thread, assumption *FaultAssumptionDef, kwargs []starlark.Tuple) (starlark.Value, error) {
	// Extract run= callback.
	var bodyFn starlark.Callable
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "run" {
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault() run= must be a callable")
			}
			bodyFn = cb
		}
	}
	if bodyFn == nil {
		return nil, fmt.Errorf("fault() requires run= keyword with a callback function")
	}

	// Apply syscall-level faults grouped by target service.
	faultsByService := make(map[string]map[string]*FaultDef)
	for _, r := range assumption.Rules {
		svcName := r.Target.Name
		if faultsByService[svcName] == nil {
			faultsByService[svcName] = make(map[string]*FaultDef)
		}
		faultsByService[svcName][r.Syscall] = r.Fault
	}
	for svcName, faults := range faultsByService {
		if err := rt.applyFaults(svcName, faults); err != nil {
			return nil, fmt.Errorf("fault(): %w", err)
		}
		defer rt.removeFaults(svcName)
	}

	// Apply protocol-level faults via the same proxy manager that
	// builtinFaultProtocol uses for the direct fault(iface_ref, rule, run=)
	// path. Without this, fault_assumption(rules=[...]) inside
	// fault_scenario/fault_matrix is silently cosmetic — the composable API
	// surface exists but no rule ever reaches the proxy. Customer-reported
	// on v0.8.8; fix shipped in v0.9.4.
	type proxyKey struct{ svc, iface string }
	appliedProxies := make(map[proxyKey]struct{})
	for _, pr := range assumption.ProxyRules {
		if pr.Target == nil || pr.Target.Service == nil || pr.ProxyFault == nil {
			continue
		}
		svcName := pr.Target.Service.Name
		ifaceName := pr.Target.Interface.Name
		proto := pr.Target.Interface.Protocol
		port := pr.Target.Interface.Port
		if pr.Target.Interface.HostPort > 0 {
			port = pr.Target.Interface.HostPort
		}
		targetAddr := fmt.Sprintf("127.0.0.1:%d", port)
		if _, err := rt.proxyMgr.EnsureProxy(context.Background(), svcName, ifaceName, proto, targetAddr); err != nil {
			return nil, fmt.Errorf("fault() proxy start for %s.%s: %w", svcName, ifaceName, err)
		}
		rt.proxyMgr.AddRule(svcName, ifaceName, proxyFaultToRule(pr.ProxyFault))
		appliedProxies[proxyKey{svcName, ifaceName}] = struct{}{}
		rt.events.Emit("proxy_fault_applied", svcName, map[string]string{
			"interface":  ifaceName,
			"protocol":   proto,
			"assumption": assumption.Name,
		})
	}
	defer func() {
		for k := range appliedProxies {
			rt.proxyMgr.ClearRules(k.svc, k.iface)
			rt.events.Emit("proxy_fault_removed", k.svc, map[string]string{
				"interface":  k.iface,
				"assumption": assumption.Name,
			})
		}
	}()

	// Register monitors for the duration of the callback.
	var monitorIDs []int
	for _, m := range assumption.Monitors {
		monitorIDs = append(monitorIDs, rt.RegisterMonitor(m))
	}
	defer func() {
		for _, id := range monitorIDs {
			rt.UnregisterMonitor(id)
		}
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

// fault_all([svc1, svc2, ...], connect=deny("ECONNREFUSED"), run=scenario)
// Applies the same fault kwargs to all listed services simultaneously.
func (rt *Runtime) builtinFaultAll(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault_all() requires a list of services as the first argument")
	}
	svcList, ok := args[0].(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("fault_all() first arg must be a list of services, got %s", args[0].Type())
	}
	if svcList.Len() == 0 {
		return nil, fmt.Errorf("fault_all() requires at least one service in the list")
	}

	// Collect services from the list.
	services := make([]*ServiceDef, 0, svcList.Len())
	for i := 0; i < svcList.Len(); i++ {
		svc, ok := svcList.Index(i).(*ServiceDef)
		if !ok {
			return nil, fmt.Errorf("fault_all() services[%d] must be a service, got %s", i, svcList.Index(i).Type())
		}
		services = append(services, svc)
	}

	// Extract run= callback and fault specs from kwargs.
	var bodyFn starlark.Callable
	faults := make(map[string]*FaultDef)
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "run" {
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_all() run= must be a callable")
			}
			bodyFn = cb
		} else {
			fd, ok := kv[1].(*FaultDef)
			if !ok {
				return nil, fmt.Errorf("fault_all() %s= must be a fault (delay/deny), got %s", key, kv[1].Type())
			}
			faults[key] = fd
		}
	}

	if bodyFn == nil {
		return nil, fmt.Errorf("fault_all() requires run= keyword with a callback function")
	}

	// Apply the same faults to all services.
	for _, svc := range services {
		if err := rt.applyFaults(svc.Name, faults); err != nil {
			return nil, fmt.Errorf("fault_all(): %w", err)
		}
		defer rt.removeFaults(svc.Name)
	}

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

// callerPosition returns the file path and 1-based line number of the
// Starlark frame that called the current builtin. The 0th frame is
// the builtin itself, so the caller lives at depth 1. Used by the
// assert_* builtins to attach a source location to AssertionDetail so
// the report renderer can lift the original assertion expression out
// of the bundled spec.
func callerPosition(thread *starlark.Thread) (string, int32) {
	if thread.CallStackDepth() < 2 {
		return "", 0
	}
	frame := thread.CallFrame(1)
	return frame.Pos.Filename(), int32(frame.Pos.Line)
}

// assert_true(condition, msg=)
func (rt *Runtime) builtinAssertTrue(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("assert_true() requires a condition")
	}
	if !args[0].Truth() {
		msg := "assertion failed"
		if len(args) > 1 {
			msg, _ = starlark.AsString(args[1])
		}
		file, line := callerPosition(thread)
		rt.lastAssertion = &AssertionDetail{
			Func:     "assert_true",
			Expected: "True",
			Actual:   args[0].String(),
			Message:  msg,
			File:     file,
			Line:     line,
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return starlark.None, nil
}

// assert_eq(a, b, msg=)
func (rt *Runtime) builtinAssertEq(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("assert_eq() requires two arguments")
	}
	eq, err := starlark.Equal(args[0], args[1])
	if err != nil {
		return nil, err
	}
	if !eq {
		var custom string
		if len(args) > 2 {
			custom, _ = starlark.AsString(args[2])
		}
		// Convention: assert_eq(actual, expected, msg). Aligns with the
		// drill-down rendering and matches what users naturally type
		// when checking response codes / state values.
		actual := args[0].String()
		expected := args[1].String()
		msg := fmt.Sprintf("assert_eq failed: %s != %s", actual, expected)
		if custom != "" {
			msg = fmt.Sprintf("assert_eq failed: %s != %s (%s)", actual, expected, custom)
		}
		file, line := callerPosition(thread)
		rt.lastAssertion = &AssertionDetail{
			Func:     "assert_eq",
			Expected: expected,
			Actual:   actual,
			Message:  custom,
			File:     file,
			Line:     line,
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
// Supports: trailing * (prefix), leading * (suffix), middle * (filepath.Match glob).
func matchValue(actual, pattern string) bool {
	if pattern == "" {
		return true
	}
	// If pattern contains *, try glob matching (covers prefix, suffix, and middle *).
	if strings.Contains(pattern, "*") {
		matched, err := filepath.Match(pattern, actual)
		if err == nil && matched {
			return true
		}
		// Fallback: also try as substring for leading/trailing * convenience.
		if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(actual, strings.TrimSuffix(pattern, "*"))
		}
		if strings.HasPrefix(pattern, "*") {
			return strings.HasSuffix(actual, strings.TrimPrefix(pattern, "*"))
		}
		return false
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

// fault_assumption(name, target=, **syscall_faults, rules=, monitors=, faults=, description=)
// Creates a named, reusable fault configuration. Returns a FaultAssumptionDef.
func (rt *Runtime) builtinFaultAssumption(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	// Parse name — first positional arg.
	if len(args) < 1 {
		return nil, fmt.Errorf("fault_assumption() requires a name")
	}
	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("fault_assumption() name must be a string, got %s", args[0].Type())
	}

	// Parse kwargs.
	var target starlark.Value   // *ServiceDef or *InterfaceRef
	var description string
	var monitors []*MonitorDef
	var children []*FaultAssumptionDef
	var proxyRules []*ProxyFaultDef
	syscallFaults := make(map[string]*FaultDef)

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "target":
			target = kv[1]
		case "description":
			s, _ := starlark.AsString(kv[1])
			description = s
		case "monitors":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_assumption() monitors= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				m, ok := list.Index(i).(*MonitorDef)
				if !ok {
					return nil, fmt.Errorf("fault_assumption() monitors[%d] must be a monitor, got %s", i, list.Index(i).Type())
				}
				monitors = append(monitors, m)
			}
		case "faults":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_assumption() faults= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				child, ok := list.Index(i).(*FaultAssumptionDef)
				if !ok {
					return nil, fmt.Errorf("fault_assumption() faults[%d] must be a fault_assumption, got %s", i, list.Index(i).Type())
				}
				children = append(children, child)
			}
		case "rules":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_assumption() rules= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				pf, ok := list.Index(i).(*ProxyFaultDef)
				if !ok {
					return nil, fmt.Errorf("fault_assumption() rules[%d] must be a proxy fault (response/error/drop/...), got %s", i, list.Index(i).Type())
				}
				proxyRules = append(proxyRules, pf)
			}
		default:
			// Syscall fault kwarg.
			fd, ok := kv[1].(*FaultDef)
			if !ok {
				return nil, fmt.Errorf("fault_assumption() %s= must be a fault (delay/deny/allow), got %s", key, kv[1].Type())
			}
			syscallFaults[key] = fd
		}
	}

	assumption := &FaultAssumptionDef{
		Name:        name,
		Description: description,
		Monitors:    monitors,
	}

	// Resolve syscall faults against the target.
	if len(syscallFaults) > 0 {
		if target == nil {
			return nil, fmt.Errorf("fault_assumption() requires target= when syscall faults are specified")
		}
		svc, isSvc := target.(*ServiceDef)
		ifRef, isIfRef := target.(*InterfaceRef)
		if isIfRef {
			svc = ifRef.Service
		}
		if !isSvc && !isIfRef {
			return nil, fmt.Errorf("fault_assumption() target= must be a service or interface_ref, got %s", target.Type())
		}

		for key, fd := range syscallFaults {
			// Resolution order: named op → family → raw syscall.
			if svc.Ops != nil {
				if opDef, isOp := svc.Ops[key]; isOp {
					for _, sc := range opDef.Syscalls {
						opFd := *fd
						opFd.Op = key
						if opDef.Path != "" {
							opFd.PathGlob = opDef.Path
						}
						for _, expanded := range expandSyscallFamily(sc) {
							assumption.Rules = append(assumption.Rules, FaultAssumptionRule{
								Target:  svc,
								Syscall: expanded,
								Fault:   &opFd,
							})
						}
					}
					continue
				}
			}
			// Family or raw syscall expansion.
			for _, expanded := range expandSyscallFamily(key) {
				copyFd := *fd
				assumption.Rules = append(assumption.Rules, FaultAssumptionRule{
					Target:  svc,
					Syscall: expanded,
					Fault:   &copyFd,
				})
			}
		}
	}

	// Resolve protocol-level faults.
	if len(proxyRules) > 0 {
		if target == nil {
			return nil, fmt.Errorf("fault_assumption() requires target= when rules= is specified")
		}
		ifRef, ok := target.(*InterfaceRef)
		if !ok {
			return nil, fmt.Errorf("fault_assumption() rules= requires target to be an interface_ref (e.g., service.interface), got %s", target.Type())
		}
		for _, pf := range proxyRules {
			assumption.ProxyRules = append(assumption.ProxyRules, FaultAssumptionProxyRule{
				Target:     ifRef,
				ProxyFault: pf,
			})
		}
	}

	// Merge children (composition).
	for _, child := range children {
		// Rules: append (last-wins on conflict handled at apply time).
		assumption.Rules = append(assumption.Rules, child.Rules...)
		assumption.ProxyRules = append(assumption.ProxyRules, child.ProxyRules...)
		// Monitors: inherit from children.
		assumption.Monitors = append(assumption.Monitors, child.Monitors...)
	}

	// Register in the runtime for string-based lookup.
	if rt.faultAssumptions == nil {
		rt.faultAssumptions = make(map[string]*FaultAssumptionDef)
	}
	rt.faultAssumptions[name] = assumption

	return assumption, nil
}

// fault_scenario(name, scenario=, faults=, expect=, monitors=, timeout=)
// Composes a scenario probe with fault assumptions and an oracle.
// Registers a test as test_<name>.
func (rt *Runtime) builtinFaultScenario(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fault_scenario() requires a name")
	}
	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("fault_scenario() name must be a string, got %s", args[0].Type())
	}

	fs := &FaultScenarioDef{
		Name:    name,
		Timeout: 30 * time.Second,
	}

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "scenario":
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_scenario() scenario= must be a callable, got %s", kv[1].Type())
			}
			fs.Scenario = cb
		case "faults":
			// Accept single FaultAssumptionDef or list.
			switch v := kv[1].(type) {
			case *FaultAssumptionDef:
				fs.Faults = []*FaultAssumptionDef{v}
			case *starlark.List:
				for i := 0; i < v.Len(); i++ {
					fa, ok := v.Index(i).(*FaultAssumptionDef)
					if !ok {
						return nil, fmt.Errorf("fault_scenario() faults[%d] must be a fault_assumption, got %s", i, v.Index(i).Type())
					}
					fs.Faults = append(fs.Faults, fa)
				}
			default:
				return nil, fmt.Errorf("fault_scenario() faults= must be a fault_assumption or list, got %s", kv[1].Type())
			}
		case "expect":
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_scenario() expect= must be a callable, got %s", kv[1].Type())
			}
			fs.Expect = cb
		case "monitors":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_scenario() monitors= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				m, ok := list.Index(i).(*MonitorDef)
				if !ok {
					return nil, fmt.Errorf("fault_scenario() monitors[%d] must be a monitor, got %s", i, list.Index(i).Type())
				}
				fs.Monitors = append(fs.Monitors, m)
			}
		case "timeout":
			s, _ := starlark.AsString(kv[1])
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("fault_scenario() timeout= invalid duration %q: %w", s, err)
			}
			fs.Timeout = d
		default:
			return nil, fmt.Errorf("fault_scenario() unexpected keyword %q", key)
		}
	}

	if fs.Scenario == nil {
		return nil, fmt.Errorf("fault_scenario() requires scenario= keyword")
	}

	// Register in runtime.
	if rt.faultScenarios == nil {
		rt.faultScenarios = make(map[string]*FaultScenarioDef)
	}
	rt.faultScenarios[name] = fs

	return starlark.None, nil
}

// fault_matrix(scenarios=, faults=, default_expect=, overrides={}, monitors=[], exclude=[])
// Generates the cross-product of scenarios × fault assumptions.
// Each cell becomes a fault_scenario registered as test_matrix_<scenario>_<fault>.
func (rt *Runtime) builtinFaultMatrix(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var scenarios []starlark.Callable
	var faults []*FaultAssumptionDef
	var defaultExpect starlark.Callable
	var monitors []*MonitorDef
	var overridesDict *starlark.Dict
	var excludeList *starlark.List
	var requireFaultsFire bool

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "scenarios":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() scenarios= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				cb, ok := list.Index(i).(starlark.Callable)
				if !ok {
					return nil, fmt.Errorf("fault_matrix() scenarios[%d] must be callable, got %s", i, list.Index(i).Type())
				}
				scenarios = append(scenarios, cb)
			}
		case "faults":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() faults= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				fa, ok := list.Index(i).(*FaultAssumptionDef)
				if !ok {
					return nil, fmt.Errorf("fault_matrix() faults[%d] must be a fault_assumption, got %s", i, list.Index(i).Type())
				}
				faults = append(faults, fa)
			}
		case "default_expect":
			if kv[1] != starlark.None {
				cb, ok := kv[1].(starlark.Callable)
				if !ok {
					return nil, fmt.Errorf("fault_matrix() default_expect= must be callable or None, got %s", kv[1].Type())
				}
				defaultExpect = cb
			}
		case "overrides":
			d, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() overrides= must be a dict, got %s", kv[1].Type())
			}
			overridesDict = d
		case "monitors":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() monitors= must be a list, got %s", kv[1].Type())
			}
			for i := 0; i < list.Len(); i++ {
				m, ok := list.Index(i).(*MonitorDef)
				if !ok {
					return nil, fmt.Errorf("fault_matrix() monitors[%d] must be a monitor, got %s", i, list.Index(i).Type())
				}
				monitors = append(monitors, m)
			}
		case "exclude":
			l, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() exclude= must be a list, got %s", kv[1].Type())
			}
			excludeList = l
		case "require_faults_fire":
			b, ok := kv[1].(starlark.Bool)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() require_faults_fire= must be bool, got %s", kv[1].Type())
			}
			requireFaultsFire = bool(b)
		default:
			return nil, fmt.Errorf("fault_matrix() unexpected keyword %q", key)
		}
	}

	if len(scenarios) == 0 {
		return nil, fmt.Errorf("fault_matrix() requires scenarios= with at least one scenario")
	}
	if len(faults) == 0 {
		return nil, fmt.Errorf("fault_matrix() requires faults= with at least one fault_assumption")
	}

	// Build exclude set for fast lookup.
	type cellKey struct {
		scenarioName string
		faultName    string
	}
	excluded := make(map[cellKey]bool)
	if excludeList != nil {
		for i := 0; i < excludeList.Len(); i++ {
			tup, ok := excludeList.Index(i).(starlark.Tuple)
			if !ok || len(tup) != 2 {
				return nil, fmt.Errorf("fault_matrix() exclude[%d] must be a (scenario, fault) tuple", i)
			}
			sc, ok := tup[0].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() exclude[%d][0] must be callable", i)
			}
			fa, ok := tup[1].(*FaultAssumptionDef)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() exclude[%d][1] must be a fault_assumption", i)
			}
			excluded[cellKey{sc.Name(), fa.Name}] = true
		}
	}

	// Build overrides map: (scenario_name, fault_name) → expect callable.
	overrides := make(map[cellKey]starlark.Callable)
	if overridesDict != nil {
		for _, item := range overridesDict.Items() {
			tup, ok := item[0].(starlark.Tuple)
			if !ok || len(tup) != 2 {
				return nil, fmt.Errorf("fault_matrix() overrides key must be a (scenario, fault) tuple")
			}
			sc, ok := tup[0].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() overrides key[0] must be callable")
			}
			fa, ok := tup[1].(*FaultAssumptionDef)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() overrides key[1] must be a fault_assumption")
			}
			expect, ok := item[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault_matrix() overrides value must be callable")
			}
			overrides[cellKey{sc.Name(), fa.Name}] = expect
		}
	}

	// Generate cross-product.
	if rt.faultScenarios == nil {
		rt.faultScenarios = make(map[string]*FaultScenarioDef)
	}

	for _, sc := range scenarios {
		for _, fa := range faults {
			ck := cellKey{sc.Name(), fa.Name}
			if excluded[ck] {
				continue
			}

			name := "matrix_" + sc.Name() + "_" + fa.Name
			expect := defaultExpect
			if override, ok := overrides[ck]; ok {
				expect = override
			}

			fs := &FaultScenarioDef{
				Name:              name,
				Scenario:          sc,
				Faults:            []*FaultAssumptionDef{fa},
				Expect:            expect,
				Monitors:          monitors, // matrix-wide monitors
				Timeout:           30 * time.Second,
				Matrix:            &MatrixInfo{ScenarioName: sc.Name(), FaultName: fa.Name},
				RequireFaultsFire: requireFaultsFire,
			}
			rt.faultScenarios[name] = fs
		}
	}

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
// Creates a MonitorDef — a first-class monitor value.
// When called inside a running test (inTest=true), also auto-registers
// the monitor on the event log for backward compatibility.
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
	var filters []EventFilter
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		val, _ := starlark.AsString(kv[1])
		filters = append(filters, EventFilter{Key: key, Value: val})
	}

	m := &MonitorDef{Callback: callback, Filters: filters}

	// Auto-register when called inside a running test (backward compat).
	if rt.inTest {
		rt.RegisterMonitor(m)
	}

	return m, nil
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
// ---------------------------------------------------------------------------
// Protocol-level fault builtins
// ---------------------------------------------------------------------------

// builtinFaultProtocol handles fault(interface_ref, *proxy_faults, run=fn).
func (rt *Runtime) builtinFaultProtocol(thread *starlark.Thread, ifRef *InterfaceRef, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	svcName := ifRef.Service.Name
	ifaceName := ifRef.Interface.Name
	proto := ifRef.Interface.Protocol

	// Extract run= and source= from kwargs, rest are ignored for protocol faults.
	var bodyFn starlark.Callable
	var sourceSvc string
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		if key == "run" {
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return nil, fmt.Errorf("fault() run= must be a callable")
			}
			bodyFn = cb
		} else if key == "source" {
			if s, ok := kv[1].(*ServiceDef); ok {
				sourceSvc = s.Name
			}
		}
	}

	if bodyFn == nil {
		return nil, fmt.Errorf("fault() requires run= keyword with a callback function")
	}

	// Collect proxy fault defs from positional args.
	var proxyFaults []*ProxyFaultDef
	for _, arg := range args {
		pf, ok := arg.(*ProxyFaultDef)
		if !ok {
			return nil, fmt.Errorf("fault(interface_ref, ...) arguments must be response()/error()/drop(), got %s", arg.Type())
		}
		proxyFaults = append(proxyFaults, pf)
	}

	if len(proxyFaults) == 0 {
		return nil, fmt.Errorf("fault(interface_ref, ...) requires at least one protocol fault (response, error, drop, etc.)")
	}

	// Resolve target address.
	port := ifRef.Interface.Port
	if ifRef.Interface.HostPort > 0 {
		port = ifRef.Interface.HostPort
	}
	targetAddr := fmt.Sprintf("127.0.0.1:%d", port)

	// Ensure proxy is running for this interface.
	proxyAddr, err := rt.proxyMgr.EnsureProxy(context.Background(), svcName, ifaceName, proto, targetAddr)
	if err != nil {
		return nil, fmt.Errorf("fault(): %w", err)
	}

	// Convert ProxyFaultDefs to proxy.Rules and add them.
	for _, pf := range proxyFaults {
		rule := proxyFaultToRule(pf)
		rt.proxyMgr.AddRule(svcName, ifaceName, rule)
	}

	// Emit event.
	rt.events.Emit("proxy_fault_applied", svcName, map[string]string{
		"interface": ifaceName,
		"protocol":  proto,
		"proxy":     proxyAddr,
		"source":    sourceSvc,
	})

	// Run body, then clear rules.
	defer func() {
		rt.proxyMgr.ClearRules(svcName, ifaceName)
		rt.events.Emit("proxy_fault_removed", svcName, map[string]string{
			"interface": ifaceName,
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

// proxyFaultToRule converts a Starlark ProxyFaultDef to a proxy.Rule.
func proxyFaultToRule(pf *ProxyFaultDef) proxy.Rule {
	var action proxy.Action
	switch pf.Action {
	case "respond":
		action = proxy.ActionRespond
	case "error":
		action = proxy.ActionError
	case "delay":
		action = proxy.ActionDelay
	case "drop":
		action = proxy.ActionDrop
	case "duplicate":
		action = proxy.ActionDuplicate
	}
	return proxy.Rule{
		Method:  pf.Method,
		Path:    pf.Path,
		Query:   pf.Query,
		Key:     pf.Key,
		Topic:   pf.Topic,
		Command: pf.Command,
		Action:  action,
		Status:  pf.Status,
		Body:    pf.Body,
		Error:   pf.Error,
		Delay:   pf.Delay,
		Prob:    pf.Probability,
	}
}

// response(method=, path=, status=, body=) — return custom response.
func builtinProxyResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	pf := &ProxyFaultDef{Action: "respond"}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "method":
			pf.Method, _ = starlark.AsString(kv[1])
		case "path":
			pf.Path, _ = starlark.AsString(kv[1])
		case "query":
			pf.Query, _ = starlark.AsString(kv[1])
		case "key":
			pf.Key, _ = starlark.AsString(kv[1])
		case "command":
			pf.Command, _ = starlark.AsString(kv[1])
		case "topic":
			pf.Topic, _ = starlark.AsString(kv[1])
		case "status":
			if n, ok := kv[1].(starlark.Int); ok {
				val, _ := n.Int64()
				pf.Status = int(val)
			}
		case "body":
			pf.Body, _ = starlark.AsString(kv[1])
		case "value":
			pf.Body, _ = starlark.AsString(kv[1])
		case "probability":
			pf.Probability = parseProbability(kv[1])
		}
	}
	return pf, nil
}

// error(method=, path=, query=, command=, key=, op=, collection=, message=, status=) — return error.
// `op=` is an alias for `method=` and `collection=` for `key=` (natural for MongoDB/document stores).
func builtinProxyError(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	pf := &ProxyFaultDef{Action: "error"}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "method", "op":
			pf.Method, _ = starlark.AsString(kv[1])
		case "path":
			pf.Path, _ = starlark.AsString(kv[1])
		case "query":
			pf.Query, _ = starlark.AsString(kv[1])
		case "key", "collection":
			pf.Key, _ = starlark.AsString(kv[1])
		case "command":
			pf.Command, _ = starlark.AsString(kv[1])
		case "topic":
			pf.Topic, _ = starlark.AsString(kv[1])
		case "message":
			pf.Error, _ = starlark.AsString(kv[1])
		case "status":
			if n, ok := kv[1].(starlark.Int); ok {
				val, _ := n.Int64()
				pf.Status = int(val)
			}
		case "probability":
			pf.Probability = parseProbability(kv[1])
		}
	}
	return pf, nil
}

// drop(method=, path=, topic=, op=, collection=, probability=) — close connection / drop message.
func builtinProxyDrop(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	pf := &ProxyFaultDef{Action: "drop"}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "method", "op":
			pf.Method, _ = starlark.AsString(kv[1])
		case "path":
			pf.Path, _ = starlark.AsString(kv[1])
		case "key", "collection":
			pf.Key, _ = starlark.AsString(kv[1])
		case "topic":
			pf.Topic, _ = starlark.AsString(kv[1])
		case "probability":
			pf.Probability = parseProbability(kv[1])
		}
	}
	return pf, nil
}

// duplicate(topic=) — deliver message twice.
func builtinProxyDuplicate(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	pf := &ProxyFaultDef{Action: "duplicate"}
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "topic":
			pf.Topic, _ = starlark.AsString(kv[1])
		}
	}
	return pf, nil
}

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

// isFaultableSyscall returns true if the given name is a known faultable syscall.
func isFaultableSyscall(name string) bool {
	for _, sc := range faultableSyscalls {
		if sc == name {
			return true
		}
	}
	return false
}
