package protocol

import "context"

// MockHandler is an optional capability on a Protocol plugin. Protocols that
// support mock_service() implement it; others do not and mock_service()
// rejects their interface type at spec load time.
//
// Serve starts an in-process listener on addr and serves the given MockSpec
// until ctx is cancelled. Each handled request MUST emit one event via emit
// so spec-level assertions see the traffic.
type MockHandler interface {
	Protocol
	ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error
}

// MockSpec is the protocol-agnostic configuration for one mocked interface.
// Routes are keyed by a protocol-specific pattern ("METHOD /path" for HTTP,
// "/pkg.Service/Method" for gRPC, bytes prefix for TCP/UDP).
type MockSpec struct {
	Routes  []MockRoute
	Default *MockResponse
}

// MockRoute pairs a pattern with a handler. Pattern matching is
// protocol-specific; handlers may be static (Response) or dynamic (Dynamic).
type MockRoute struct {
	Pattern  string
	Response *MockResponse // static response; nil if Dynamic is set
	Dynamic  DynamicFn     // per-request callable; nil if Response is set
}

// MockResponse is a canned response. Semantics depend on protocol:
//   - HTTP/HTTP2: Status is the HTTP status code, Body is the body, Headers
//     are response headers, ContentType sets the Content-Type header.
//   - gRPC: Status maps to gRPC status code; Body is the encoded message;
//     Headers become trailers.
//   - TCP/UDP: Status is ignored; Body is the raw bytes written back.
type MockResponse struct {
	Status      int
	Body        []byte
	Headers     map[string]string
	ContentType string
}

// MockRequest is passed to a DynamicFn. Fields are populated best-effort per
// protocol — HTTP populates all fields; TCP/UDP populate only Body.
type MockRequest struct {
	Method  string            // HTTP method; empty for non-HTTP
	Path    string            // HTTP path; empty for non-HTTP
	Headers map[string]string // HTTP request headers; nil for non-HTTP
	Query   map[string]string // HTTP query params; nil for non-HTTP
	Body    []byte            // raw request body bytes
}

// DynamicFn computes a MockResponse per request. Returning a non-nil error
// causes the mock to emit a 500-style error response and log the failure.
type DynamicFn func(req MockRequest) (*MockResponse, error)

// MockEmitter is a callback the mock handler invokes on every handled
// request so the event log records the traffic. Implementations are
// thread-safe; handlers may call emit concurrently from multiple goroutines.
type MockEmitter func(op string, fields map[string]string)
