package star

import (
	"fmt"
	"net/http"
	"strings"

	"go.starlark.net/starlark"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// mock_service(name, *interfaces, routes={}, default=None, depends_on=[])
//
// Creates a ServiceDef flagged as a mock. The runtime stands up an
// in-process handler per interface at test start instead of launching a
// binary or container. See RFC-017.
func (rt *Runtime) builtinMockService(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mock_service() requires at least a name")
	}
	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("mock_service() name must be a string")
	}

	svc := &ServiceDef{
		Name:       name,
		Interfaces: make(map[string]*InterfaceDef),
		Env:        make(map[string]string),
		Mock: &MockConfig{
			Routes:           make(map[string][]MockRouteEntry),
			Default:          make(map[string]*MockResponseValue),
			TLS:              make(map[string]bool),
			Config:           make(map[string]map[string]any),
			Descriptors:      make(map[string]*protoregistry.Files),
			OpenAPI:          make(map[string]*protocol.OpenAPISpec),
			ExampleSelection: make(map[string]string),
			Overrides:        make(map[string][]MockRouteEntry),
			Validate:         make(map[string]string),
		},
	}

	for i := 1; i < len(args); i++ {
		iface, ok := args[i].(*InterfaceDef)
		if !ok {
			return nil, fmt.Errorf("mock_service() positional arg %d must be interface() (got %s)", i, args[i].Type())
		}
		svc.Interfaces[iface.Name] = iface
	}

	// Single-interface shorthand: mock_service("auth", interface(...), routes={})
	// applies routes to that one interface. Multi-interface callers pass
	// routes={"interface_name": {...}} as nested dicts (future).
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "routes":
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("mock_service() routes must be a dict")
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w — provide routes as {interface: {...}} for multi-interface mocks", name, err)
			}
			routes, err := parseRoutesDict(dict)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q routes: %w", name, err)
			}
			svc.Mock.Routes[ifaceName] = routes
		case "default":
			resp, ok := kv[1].(*MockResponseValue)
			if !ok {
				return nil, fmt.Errorf("mock_service() default must be a mock_response (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			svc.Mock.Default[ifaceName] = resp
		case "depends_on":
			list, ok := kv[1].(*starlark.List)
			if !ok {
				return nil, fmt.Errorf("mock_service() depends_on must be a list")
			}
			iter := list.Iterate()
			defer iter.Done()
			var item starlark.Value
			for iter.Next(&item) {
				if s, ok := item.(*ServiceDef); ok {
					svc.DependsOn = append(svc.DependsOn, s.Name)
				} else if str, ok := starlark.AsString(item); ok {
					svc.DependsOn = append(svc.DependsOn, str)
				}
			}
		case "tls":
			b, ok := kv[1].(starlark.Bool)
			if !ok {
				return nil, fmt.Errorf("mock_service() tls must be a bool")
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			svc.Mock.TLS[ifaceName] = bool(b)
		case "config":
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("mock_service() config must be a dict (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			native, err := starlarkToGo(dict)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q config: %w", name, err)
			}
			cfg, ok := native.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mock_service() %q config must be a string-keyed dict", name)
			}
			svc.Mock.Config[ifaceName] = cfg
		case "openapi":
			// openapi="/path/to/spec.yaml" — load an OpenAPI 3.0 document
			// (YAML or JSON) whose paths × operations become auto-generated
			// mock routes. RFC-021 Phase 1. Loaded eagerly so parse/validation
			// errors surface at spec-load time.
			path, ok := starlark.AsString(kv[1])
			if !ok {
				return nil, fmt.Errorf("mock_service() openapi must be a path string (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			spec, err := protocol.LoadOpenAPI(path)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q openapi: %w", name, err)
			}
			svc.Mock.OpenAPI[ifaceName] = spec
		case "examples":
			// examples= selects the response-picking strategy for OpenAPI
			// route generation. Accepted values:
			//   - ""       — same as "first"
			//   - "first"  — deterministic, first declared example
			//   - "random" — seeded random per operation (reproducible)
			//   - "synthesize" — first example if present, else minimal
			//                    type-correct values from the schema
			//   - any other string — treated as a named key in the
			//                        operations' `examples:` maps
			// Validation is deferred to routes-build time where the
			// selector resolves.
			sel, ok := starlark.AsString(kv[1])
			if !ok {
				return nil, fmt.Errorf("mock_service() examples must be a string (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			svc.Mock.ExampleSelection[ifaceName] = sel
		case "overrides":
			// overrides={METHOD /path: mock_response()} — routes that
			// REPLACE OpenAPI-generated entries with the same pattern.
			// Paths accept OpenAPI-style parameters (`{id}`) which are
			// normalised to glob segments (`*`) so override keys can be
			// copied directly from the OpenAPI document. RFC-021 OQ4.
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("mock_service() overrides must be a dict (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			overrides, err := parseRoutesDict(dict)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q overrides: %w", name, err)
			}
			svc.Mock.Overrides[ifaceName] = overrides
		case "validate":
			// validate= controls request validation when openapi= is set.
			// off (default): no validation. warn: log mismatches but
			// serve anyway. strict: reject with HTTP 400. RFC-021 OQ6.
			mode, ok := starlark.AsString(kv[1])
			if !ok {
				return nil, fmt.Errorf("mock_service() validate must be a string (got %s)", kv[1].Type())
			}
			if mode != "" && mode != "off" && mode != "warn" && mode != "strict" {
				return nil, fmt.Errorf("mock_service() %q validate=%q: expected one of off/warn/strict", name, mode)
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			svc.Mock.Validate[ifaceName] = mode
		case "descriptors":
			// descriptors="/path/to/file.pb" — load a FileDescriptorSet
			// (protoc output) for typed gRPC responses. RFC-023 Phase 2.
			// Loaded eagerly so .pb parse errors surface at spec-load time.
			path, ok := starlark.AsString(kv[1])
			if !ok {
				return nil, fmt.Errorf("mock_service() descriptors must be a path string (got %s)", kv[1].Type())
			}
			ifaceName, err := singleInterfaceName(svc)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q: %w", name, err)
			}
			files, err := protocol.LoadDescriptorSet(path)
			if err != nil {
				return nil, fmt.Errorf("mock_service() %q descriptors: %w", name, err)
			}
			svc.Mock.Descriptors[ifaceName] = files
		}
	}

	if len(svc.Interfaces) == 0 {
		return nil, fmt.Errorf("mock_service() %q requires at least one interface", name)
	}

	// Validate every interface declares a protocol that implements MockHandler.
	for _, iface := range svc.Interfaces {
		p, ok := protocol.Get(iface.Protocol)
		if !ok {
			return nil, fmt.Errorf("mock_service() %q: unknown protocol %q", name, iface.Protocol)
		}
		if _, ok := p.(protocol.MockHandler); !ok {
			return nil, fmt.Errorf("mock_service() %q: protocol %q does not support mock_service() yet", name, iface.Protocol)
		}
	}

	rt.registerService(svc)
	return svc, nil
}

func singleInterfaceName(svc *ServiceDef) (string, error) {
	if len(svc.Interfaces) != 1 {
		return "", fmt.Errorf("mock_service has %d interfaces, disambiguation required", len(svc.Interfaces))
	}
	for name := range svc.Interfaces {
		return name, nil
	}
	return "", fmt.Errorf("mock_service has no interfaces")
}

func parseRoutesDict(d *starlark.Dict) ([]MockRouteEntry, error) {
	out := make([]MockRouteEntry, 0, d.Len())
	for _, pair := range d.Items() {
		pattern, ok := starlark.AsString(pair[0])
		if !ok {
			return nil, fmt.Errorf("route keys must be strings (got %s)", pair[0].Type())
		}
		resp, ok := pair[1].(*MockResponseValue)
		if !ok {
			return nil, fmt.Errorf("route %q: value must be a mock_response (got %s)", pattern, pair[1].Type())
		}
		out = append(out, MockRouteEntry{
			Pattern:  normaliseRoutePattern(pattern),
			Response: resp,
		})
	}
	return out, nil
}

// normaliseRoutePattern converts OpenAPI-style path parameters (`{name}`) in
// the path portion of a route pattern into glob segments (`*`) understood by
// the HTTP mock matcher. Patterns without `{` are returned unchanged so
// existing `METHOD /foo/*` usage stays byte-identical. The method portion
// (before the first space) is left alone.
func normaliseRoutePattern(pattern string) string {
	sp := strings.IndexByte(pattern, ' ')
	if sp < 0 || !strings.Contains(pattern[sp+1:], "{") {
		return pattern
	}
	return pattern[:sp+1] + protocol.OpenAPIPathToGlob(pattern[sp+1:])
}

// json_response(status, body, headers={}) — encodes body as JSON.
func builtinJSONResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		statusVal starlark.Value = starlark.MakeInt(200)
		body      starlark.Value = starlark.None
		headers   *starlark.Dict
	)
	if err := starlark.UnpackArgs("json_response", args, kwargs, "status?", &statusVal, "body?", &body, "headers?", &headers); err != nil {
		return nil, err
	}

	status, err := starInt(statusVal)
	if err != nil {
		return nil, fmt.Errorf("json_response status: %w", err)
	}
	encoded, err := marshalJSONBody(body)
	if err != nil {
		return nil, fmt.Errorf("json_response body: %w", err)
	}
	resp := &protocol.MockResponse{
		Status:      status,
		Body:        encoded,
		ContentType: "application/json",
		Headers:     headerDictToMap(headers),
	}
	return newStaticResponse(resp), nil
}

// text_response(status, body, headers={}) — returns plain text.
func builtinTextResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		statusVal starlark.Value = starlark.MakeInt(200)
		body      string
		headers   *starlark.Dict
	)
	if err := starlark.UnpackArgs("text_response", args, kwargs, "status?", &statusVal, "body?", &body, "headers?", &headers); err != nil {
		return nil, err
	}
	status, err := starInt(statusVal)
	if err != nil {
		return nil, fmt.Errorf("text_response status: %w", err)
	}
	resp := &protocol.MockResponse{
		Status:      status,
		Body:        []byte(body),
		ContentType: "text/plain; charset=utf-8",
		Headers:     headerDictToMap(headers),
	}
	return newStaticResponse(resp), nil
}

// bytes_response(status, data) — returns raw bytes. For TCP/UDP mocks.
func builtinBytesResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		statusVal starlark.Value = starlark.MakeInt(0)
		data      string
	)
	if err := starlark.UnpackArgs("bytes_response", args, kwargs, "status?", &statusVal, "data?", &data); err != nil {
		return nil, err
	}
	status, err := starInt(statusVal)
	if err != nil {
		return nil, fmt.Errorf("bytes_response status: %w", err)
	}
	resp := &protocol.MockResponse{
		Status: status,
		Body:   []byte(data),
	}
	return newStaticResponse(resp), nil
}

// status_only(code) — HTTP response with a status and empty body.
func builtinStatusOnly(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var code int
	if err := starlark.UnpackArgs("status_only", args, kwargs, "code", &code); err != nil {
		return nil, err
	}
	resp := &protocol.MockResponse{Status: code}
	return newStaticResponse(resp), nil
}

// redirect(location, status=302) — HTTP redirect with Location header.
func builtinRedirect(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		location string
		status   = http.StatusFound
	)
	if err := starlark.UnpackArgs("redirect", args, kwargs, "location", &location, "status?", &status); err != nil {
		return nil, err
	}
	resp := &protocol.MockResponse{
		Status:  status,
		Headers: map[string]string{"Location": location},
	}
	return newStaticResponse(resp), nil
}

// dynamic(fn) — wraps a Starlark callable as a mock response. The callable
// receives a request dict and must return a mock_response value.
func builtinDynamic(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var handler starlark.Value
	if err := starlark.UnpackArgs("dynamic", args, kwargs, "fn", &handler); err != nil {
		return nil, err
	}
	callable, ok := handler.(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("dynamic() requires a callable (got %s)", handler.Type())
	}
	return newDynamicResponse(callable), nil
}

// grpc_response(body) — returns a gRPC response message. Body is a dict
// (or list) that gets encoded as google.protobuf.Struct — the common
// wire format for typeless gRPC mocks. Typed clients using reflection or
// loose decoding (most Go / Node clients) accept this shape.
func builtinGRPCResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var body starlark.Value = starlark.None
	if err := starlark.UnpackArgs("grpc_response", args, kwargs, "body?", &body); err != nil {
		return nil, err
	}
	jsonBytes, err := marshalJSONBody(body)
	if err != nil {
		return nil, fmt.Errorf("grpc_response body: %w", err)
	}
	structBytes, err := protocol.JSONToGRPCStruct(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("grpc_response encode: %w", err)
	}
	return newStaticResponse(&protocol.MockResponse{
		Status: 0, // OK
		Body:   structBytes,
	}), nil
}

// grpc_typed_response(body) — returns a gRPC response whose body is the
// raw JSON representation of a typed proto message. Only usable with
// mock_service(..., descriptors="x.pb"); the gRPC handler resolves the
// per-method output descriptor at request-time and encodes the JSON
// against it. Use via @faultbox/mocks/grpc.star's grpc.server() wrapper.
// RFC-023 Phase 2.
func builtinGRPCTypedResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var body starlark.Value = starlark.None
	if err := starlark.UnpackArgs("grpc_typed_response", args, kwargs, "body?", &body); err != nil {
		return nil, err
	}
	jsonBytes, err := marshalJSONBody(body)
	if err != nil {
		return nil, fmt.Errorf("grpc_typed_response body: %w", err)
	}
	return newStaticResponse(&protocol.MockResponse{
		Status: 0,
		Body:   jsonBytes, // encoded at request time against the method's output descriptor
	}), nil
}

// grpc_raw_response(bytes) — returns a gRPC response whose body is
// pre-encoded wire bytes, bypassing the typed encoder entirely. Escape
// hatch for cases where the typed encoder can't express what the
// customer needs (oneof tricks, deprecated fields, extensions).
// Power-user path. RFC-023 Phase 2.
func builtinGRPCRawResponse(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var raw starlark.String
	if err := starlark.UnpackArgs("grpc_raw_response", args, kwargs, "body", &raw); err != nil {
		return nil, err
	}
	return newStaticResponse(&protocol.MockResponse{
		Status:      0,
		Body:        []byte(string(raw)),
		ContentType: protocol.GRPCRawBodyContentType,
	}), nil
}

// grpc_error(code, message) — returns a gRPC error response. code is a
// gRPC canonical status code name (UPPERCASE, e.g. "UNAVAILABLE") or
// the numeric value. message is the human-readable error string.
func builtinGRPCError(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		codeVal starlark.Value
		message string
	)
	if err := starlark.UnpackArgs("grpc_error", args, kwargs, "code", &codeVal, "message?", &message); err != nil {
		return nil, err
	}
	var code int
	switch v := codeVal.(type) {
	case starlark.Int:
		n, ok := v.Int64()
		if !ok {
			return nil, fmt.Errorf("grpc_error code out of range")
		}
		code = int(n)
	case starlark.String:
		n, ok := protocol.GRPCCodeByName(string(v))
		if !ok {
			return nil, fmt.Errorf("grpc_error: unknown code name %q (use one of OK, CANCELLED, UNKNOWN, INVALID_ARGUMENT, DEADLINE_EXCEEDED, NOT_FOUND, ALREADY_EXISTS, PERMISSION_DENIED, RESOURCE_EXHAUSTED, FAILED_PRECONDITION, ABORTED, OUT_OF_RANGE, UNIMPLEMENTED, INTERNAL, UNAVAILABLE, DATA_LOSS, UNAUTHENTICATED)", v)
		}
		code = n
	default:
		return nil, fmt.Errorf("grpc_error code must be int or string (got %s)", codeVal.Type())
	}
	if code == 0 {
		return nil, fmt.Errorf("grpc_error code must be non-zero (0 = OK is not an error)")
	}
	return newStaticResponse(&protocol.MockResponse{
		Status: code,
		Body:   []byte(message),
	}), nil
}

// headerDictToMap is a small helper that converts a Starlark dict (or nil)
// into a Go string->string map, skipping entries that aren't string-typed.
func headerDictToMap(d *starlark.Dict) map[string]string {
	if d == nil || d.Len() == 0 {
		return nil
	}
	out := make(map[string]string, d.Len())
	for _, pair := range d.Items() {
		k, kok := starlark.AsString(pair[0])
		v, vok := starlark.AsString(pair[1])
		if kok && vok {
			out[k] = v
		}
	}
	return out
}

// starInt converts a Starlark value that might be Int or String into an int.
func starInt(v starlark.Value) (int, error) {
	if i, ok := v.(starlark.Int); ok {
		n, ok := i.Int64()
		if !ok {
			return 0, fmt.Errorf("integer out of range")
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("expected int (got %s)", v.Type())
}
