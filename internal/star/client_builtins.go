package star

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// builtinClient implements the client() topology entity (RFC-055).
//
//	client(
//	    name,                        # trace identity; must not collide with a service
//	    target = <InterfaceRef>,     # the interface to call
//	    openapi = "./api.yaml",      # OpenAPI 3.x document      } exactly one, or
//	    descriptors = "./svc.pb",    # protoc FileDescriptorSet  } inherited from
//	                                 #   interface(spec=)
//	    grpc_service = "pkg.Svc",    # required when the set declares >1 service
//	    base_path = "/v1",
//	    headers = {...},
//	    before = lambda req: req,
//	    rename = {"getOrder": "fetch_order"},
//	    validate = "off",            # off | request | response | strict
//	    timeout = "5s",
//	)
//
// Everything is resolved at spec load: the contract is parsed, the
// operation table is built, and names are checked. A client that loads is
// a client whose every operation is callable.
func (rt *Runtime) builtinClient(thread *starlark.Thread, fn *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	var (
		name        string
		targetVal   starlark.Value
		openapiPath string
		descPath    string
		grpcService string
		basePath    string
		headersVal  *starlark.Dict
		beforeVal   starlark.Value
		renameVal   *starlark.Dict
		validateStr string
		timeoutStr  string
	)
	if err := starlark.UnpackArgs("client", args, kwargs,
		"name", &name,
		"target", &targetVal,
		"openapi?", &openapiPath,
		"descriptors?", &descPath,
		"grpc_service?", &grpcService,
		"base_path?", &basePath,
		"headers?", &headersVal,
		"before?", &beforeVal,
		"rename?", &renameVal,
		"validate?", &validateStr,
		"timeout?", &timeoutStr,
	); err != nil {
		return nil, err
	}

	if name == "" {
		return nil, fmt.Errorf("client() requires a non-empty name")
	}
	target, ok := targetVal.(*InterfaceRef)
	if !ok {
		return nil, fmt.Errorf("client(%q) target= must be a service interface like `orders.public` (got %s)",
			name, targetVal.Type())
	}

	// A client and a service share the lane and vector-clock namespace, so
	// a shared name would silently fold two actors into one participant —
	// exactly the ambiguity clients exist to remove (RFC-055 OQ-8).
	rt.mu.Lock()
	_, serviceExists := rt.services[name]
	rt.mu.Unlock()
	if serviceExists {
		return nil, fmt.Errorf("client(%q): a service is already named %q\n"+
			"  clients and services share the trace's lane and vector-clock namespace; pick a distinct name",
			name, name)
	}
	if name == "test" {
		return nil, fmt.Errorf("client(%q): \"test\" is the test driver's own trace identity; pick another name", name)
	}
	if existing, dup := rt.clients[name]; dup {
		return nil, fmt.Errorf("client(%q) is already declared (bound to %s.%s)",
			name, existing.Target.Service.Name, existing.Target.Interface.Name)
	}

	validate, err := parseValidateMode(name, validateStr)
	if err != nil {
		return nil, err
	}

	var timeout time.Duration
	if timeoutStr != "" {
		timeout, err = parseStarDuration(timeoutStr)
		if err != nil {
			return nil, fmt.Errorf("client(%q) timeout=%q: %w", name, timeoutStr, err)
		}
	}

	headers, err := stringDictArg(name, "headers", headersVal)
	if err != nil {
		return nil, err
	}
	rename, err := stringDictArg(name, "rename", renameVal)
	if err != nil {
		return nil, err
	}

	var before starlark.Callable
	if beforeVal != nil && beforeVal != starlark.None {
		before, ok = beforeVal.(starlark.Callable)
		if !ok {
			return nil, fmt.Errorf("client(%q) before= must be callable (got %s)", name, beforeVal.Type())
		}
	}

	table, err := rt.buildClientTable(name, target, openapiPath, descPath, grpcService, rename)
	if err != nil {
		return nil, err
	}

	// An operation whose canonical name shadows a ClientVal attribute
	// would be unreachable through the generated surface.
	for _, opName := range table.Names() {
		if purpose, clash := clientReservedAttrs[opName]; clash {
			return nil, fmt.Errorf(
				"client(%q): operation %q shadows the built-in client attribute %q (%s)\n"+
					"  fix: rename={\"<contract name>\": \"<other_name>\"}",
				name, opName, opName, purpose)
		}
	}

	c := &ClientVal{
		Name:     name,
		Target:   target,
		Table:    table,
		BasePath: basePath,
		Headers:  headers,
		Before:   before,
		Validate: validate,
		Timeout:  timeout,
		runtime:  rt,
	}

	rt.mu.Lock()
	if rt.clients == nil {
		rt.clients = make(map[string]*ClientVal)
	}
	rt.clients[name] = c
	rt.mu.Unlock()

	return c, nil
}

// buildClientTable resolves the contract source and builds the operation
// table. Exactly one contract may be supplied explicitly; omitting both
// falls back to the target interface's `spec=`, which until RFC-055 was
// parsed and read by nothing.
func (rt *Runtime) buildClientTable(name string, target *InterfaceRef,
	openapiPath, descPath, grpcService string, rename map[string]string) (*protocol.OperationTable, error) {

	if openapiPath != "" && descPath != "" {
		return nil, fmt.Errorf("client(%q): pass either openapi= or descriptors=, not both", name)
	}

	inherited := false
	if openapiPath == "" && descPath == "" {
		spec := strings.TrimSpace(target.Interface.Spec)
		if spec == "" {
			return nil, fmt.Errorf(
				"client(%q): no contract\n"+
					"  pass openapi=\"…\" or descriptors=\"…\", or declare it once on the interface:\n"+
					"    interface(%q, %q, %d, spec=\"./contract\")",
				name, target.Interface.Name, target.Interface.Protocol, target.Interface.Port)
		}
		inherited = true
		switch strings.ToLower(filepath.Ext(spec)) {
		case ".yaml", ".yml", ".json":
			openapiPath = spec
		case ".pb", ".desc", ".protoset":
			descPath = spec
		default:
			return nil, fmt.Errorf(
				"client(%q): cannot tell what kind of contract %q is from its extension\n"+
					"  pass openapi= or descriptors= explicitly (.yaml/.yml/.json → OpenAPI, .pb/.desc → descriptor set)",
				name, spec)
		}
	}

	if openapiPath != "" {
		resolved := rt.resolveSpecPath(openapiPath)
		spec, err := protocol.LoadOpenAPI(resolved)
		if err != nil {
			return nil, fmt.Errorf("client(%q) openapi%s: %w", name, inheritedNote(inherited), err)
		}
		table, err := protocol.BuildOpenAPIOperations(spec, rename)
		if err != nil {
			return nil, fmt.Errorf("client(%q): %w", name, err)
		}
		if grpcService != "" {
			return nil, fmt.Errorf("client(%q): grpc_service= applies to descriptors=, not openapi=", name)
		}
		return table, nil
	}

	resolved := rt.resolveSpecPath(descPath)
	files, err := protocol.LoadDescriptorSet(resolved)
	if err != nil {
		return nil, fmt.Errorf("client(%q) descriptors%s: %w", name, inheritedNote(inherited), err)
	}
	table, err := protocol.BuildGRPCOperations(files, resolved, grpcService, rename)
	if err != nil {
		return nil, fmt.Errorf("client(%q): %w", name, err)
	}
	return table, nil
}

func inheritedNote(inherited bool) string {
	if inherited {
		return " (inherited from interface(spec=))"
	}
	return ""
}

// resolveSpecPath resolves a contract path relative to the spec's own
// directory, matching load_file() and build= rather than the process CWD.
func (rt *Runtime) resolveSpecPath(p string) string {
	if filepath.IsAbs(p) || rt.baseDir == "" {
		return p
	}
	return filepath.Join(rt.baseDir, p)
}

func parseValidateMode(clientName, s string) (ValidateMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return ValidateOff, nil
	case "request":
		return ValidateRequest, nil
	case "response":
		return ValidateResponse, nil
	case "strict":
		return ValidateStrict, nil
	default:
		return "", fmt.Errorf("client(%q) validate=%q: expected one of off, request, response, strict",
			clientName, s)
	}
}

// stringDictArg coerces a Starlark dict kwarg into a string map, naming
// the client and the kwarg on failure.
func stringDictArg(clientName, kwarg string, d *starlark.Dict) (map[string]string, error) {
	if d == nil || d.Len() == 0 {
		return nil, nil
	}
	out := make(map[string]string, d.Len())
	for _, pair := range d.Items() {
		k, kok := starlark.AsString(pair[0])
		v, vok := starlark.AsString(pair[1])
		if !kok || !vok {
			return nil, fmt.Errorf("client(%q) %s= keys and values must be strings (got %s: %s)",
				clientName, kwarg, pair[0].Type(), pair[1].Type())
		}
		out[k] = v
	}
	return out, nil
}

// ClientNames returns the declared client names in sorted order. Used by
// `faultbox inspect` and by the report to label driver lanes.
func (rt *Runtime) ClientNames() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	names := make([]string, 0, len(rt.clients))
	for n := range rt.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Client returns a declared client by name.
func (rt *Runtime) Client(name string) (*ClientVal, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c, ok := rt.clients[name]
	return c, ok
}
