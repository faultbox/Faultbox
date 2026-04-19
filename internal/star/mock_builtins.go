package star

import (
	"fmt"
	"net/http"

	"go.starlark.net/starlark"

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
			Routes:  make(map[string][]MockRouteEntry),
			Default: make(map[string]*MockResponseValue),
			TLS:     make(map[string]bool),
			Config:  make(map[string]map[string]any),
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
		out = append(out, MockRouteEntry{Pattern: pattern, Response: resp})
	}
	return out, nil
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
