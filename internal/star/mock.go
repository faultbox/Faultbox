package star

import (
	"encoding/json"
	"fmt"

	"go.starlark.net/starlark"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// MockConfig holds mock configuration keyed per interface. Populated by
// the mock_service() builtin; consumed by Runtime.startMockService.
type MockConfig struct {
	// Routes per interface name. Insertion order is preserved — earlier
	// routes take precedence when multiple patterns match.
	Routes map[string][]MockRouteEntry

	// Default response per interface (nil = 404 / protocol default).
	Default map[string]*MockResponseValue

	// TLS per interface (not yet implemented; field reserved).
	TLS map[string]bool

	// Config per interface — opaque protocol-specific data. Plumbs into
	// protocol.MockSpec.Config for the plugin to interpret. Used by stdlib
	// mocks (@faultbox/mocks/) to carry topics, seed state, collections,
	// etc. through the generic mock_service() primitive.
	Config map[string]map[string]any

	// Descriptors per interface — pre-parsed FileDescriptorSet registry
	// populated from the `descriptors="./path/to/x.pb"` kwarg on
	// mock_service(). Only meaningful for gRPC mocks; flipping the gRPC
	// handler from google.protobuf.Struct to typed-proto encoding (RFC-023).
	// Loaded at spec-parse time via protocol.LoadDescriptorSet so .pb
	// parse errors surface before any test runs.
	Descriptors map[string]*protoregistry.Files

	// OpenAPI per interface — pre-parsed and validated OpenAPI 3.0
	// document populated from the `openapi="./path/to/spec.yaml"` kwarg
	// on mock_service(). Only meaningful for HTTP mocks; the HTTP handler
	// auto-generates routes from the spec at startMockService time (RFC-021).
	// Loaded at spec-parse time via protocol.LoadOpenAPI so malformed
	// specs fail before any test runs.
	OpenAPI map[string]*protocol.OpenAPISpec

	// ExampleSelection per interface — name of the example-selection
	// strategy: "first" (default, deterministic), "random", "synthesize",
	// or any named-example key declared in the OpenAPI document. RFC-021.
	ExampleSelection map[string]string

	// Overrides per interface — route table that REPLACES generated routes
	// with the same pattern (listed before generated routes in the match
	// order). Patterns accept OpenAPI-style path parameters (`{id}`) and
	// are normalized to globs (`*`) at build time. RFC-021 OQ4.
	Overrides map[string][]MockRouteEntry

	// Validate per interface — request validation mode. One of "off"
	// (default, no validation), "warn" (log malformed requests, serve
	// generated response anyway), "strict" (reject with HTTP 400).
	// Only honoured when OpenAPI is set. RFC-021 OQ6.
	Validate map[string]string
}

// MockRouteEntry is one pattern → response binding.
type MockRouteEntry struct {
	Pattern  string
	Response *MockResponseValue
}

// MockResponseValue is a Starlark value returned by the response constructor
// builtins (json_response, text_response, status_only, redirect) and
// dynamic(). It wraps either a static protocol.MockResponse or a Starlark
// callable invoked per request.
type MockResponseValue struct {
	static  *protocol.MockResponse
	dynamic starlark.Callable
}

var _ starlark.Value = (*MockResponseValue)(nil)

func (m *MockResponseValue) String() string {
	if m.dynamic != nil {
		return fmt.Sprintf("<mock_response dynamic=%s>", m.dynamic.Name())
	}
	if m.static != nil {
		return fmt.Sprintf("<mock_response status=%d body_size=%d>", m.static.Status, len(m.static.Body))
	}
	return "<mock_response (empty)>"
}
func (m *MockResponseValue) Type() string          { return "mock_response" }
func (m *MockResponseValue) Freeze()               {}
func (m *MockResponseValue) Truth() starlark.Bool  { return starlark.True }
func (m *MockResponseValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: mock_response") }

// IsDynamic reports whether this response defers to a Starlark callable.
func (m *MockResponseValue) IsDynamic() bool { return m.dynamic != nil }

// Static returns the embedded static response (nil if dynamic).
func (m *MockResponseValue) Static() *protocol.MockResponse { return m.static }

// Dynamic returns the wrapped callable (nil if static).
func (m *MockResponseValue) Dynamic() starlark.Callable { return m.dynamic }

// newStaticResponse is the internal helper used by the response constructor
// builtins to wrap a protocol.MockResponse.
func newStaticResponse(r *protocol.MockResponse) *MockResponseValue {
	return &MockResponseValue{static: r}
}

// newDynamicResponse wraps a Starlark callable for per-request evaluation.
func newDynamicResponse(fn starlark.Callable) *MockResponseValue {
	return &MockResponseValue{dynamic: fn}
}

// toStarlarkRequest converts a protocol.MockRequest into the dict that
// dynamic handlers receive as their sole argument.
func toStarlarkRequest(req protocol.MockRequest) *starlark.Dict {
	d := starlark.NewDict(5)
	_ = d.SetKey(starlark.String("method"), starlark.String(req.Method))
	_ = d.SetKey(starlark.String("path"), starlark.String(req.Path))

	headers := starlark.NewDict(len(req.Headers))
	for k, v := range req.Headers {
		_ = headers.SetKey(starlark.String(k), starlark.String(v))
	}
	_ = d.SetKey(starlark.String("headers"), headers)

	query := starlark.NewDict(len(req.Query))
	for k, v := range req.Query {
		_ = query.SetKey(starlark.String(k), starlark.String(v))
	}
	_ = d.SetKey(starlark.String("query"), query)

	_ = d.SetKey(starlark.String("body"), starlark.String(string(req.Body)))
	return d
}

// starlarkResponseToProtocol converts a MockResponseValue returned by a
// dynamic handler into the wire-level protocol.MockResponse.
func starlarkResponseToProtocol(v starlark.Value) (*protocol.MockResponse, error) {
	mr, ok := v.(*MockResponseValue)
	if !ok {
		return nil, fmt.Errorf("dynamic handler must return a mock_response (got %s)", v.Type())
	}
	if mr.static == nil {
		return nil, fmt.Errorf("dynamic handler returned a dynamic response (nested dynamic() is not supported)")
	}
	return mr.static, nil
}

// marshalJSONBody encodes v as JSON bytes, used by json_response().
// Accepts Starlark dicts, lists, strings, numbers, bools, None.
func marshalJSONBody(v starlark.Value) ([]byte, error) {
	native, err := starlarkToGo(v)
	if err != nil {
		return nil, fmt.Errorf("convert value: %w", err)
	}
	return json.Marshal(native)
}

// starlarkToGo converts a Starlark value to a Go native suitable for
// json.Marshal. Inverse of goToStarlark() in types.go.
func starlarkToGo(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		return x.String(), nil
	case starlark.Float:
		return float64(x), nil
	case starlark.String:
		return string(x), nil
	case *starlark.List:
		out := make([]any, 0, x.Len())
		iter := x.Iterate()
		defer iter.Done()
		var item starlark.Value
		for iter.Next(&item) {
			g, err := starlarkToGo(item)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case starlark.Tuple:
		out := make([]any, 0, len(x))
		for _, item := range x {
			g, err := starlarkToGo(item)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, x.Len())
		for _, pair := range x.Items() {
			key, ok := pair[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("dict keys must be strings for JSON encoding (got %s)", pair[0].Type())
			}
			g, err := starlarkToGo(pair[1])
			if err != nil {
				return nil, err
			}
			out[string(key)] = g
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot JSON-encode Starlark value of type %s", v.Type())
	}
}
