package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// ServeMock implements MockHandler for gRPC. Built on google.golang.org/grpc
// with an UnknownServiceHandler — every incoming RPC is routed through the
// mock's route table, keyed by the full method path "/pkg.Service/Method".
//
// Without a .proto file, responses are encoded as google.protobuf.Struct
// (JSON-shaped). Typed clients using reflection or loose decoding (most
// Go and Node clients) accept this; clients with compiled stubs for a
// specific message type will need the real backend.
//
// Dynamic handlers receive a MockRequest with Path populated; Body is the
// raw wire bytes of the unary request message (protobuf-encoded).
//
// Error responses: grpc_error() producers set Status to a gRPC code; the
// handler translates Status to the gRPC status code with the Body as the
// human-readable message.
func (p *grpcProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mock grpc listen %s: %w", addr, err)
	}

	handler := &grpcMockHandler{routes: spec.Routes, def: spec.Default, emit: emit}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(handler.serve))

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		srv.GracefulStop()
		return nil
	case err := <-done:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
		return nil
	}
}

// grpcMockHandler dispatches unary-style RPCs against the route table.
type grpcMockHandler struct {
	routes []MockRoute
	def    *MockResponse
	emit   MockEmitter
}

// serve matches grpc.StreamHandler signature. Receives one request
// message, dispatches, sends one response (or an error).
func (h *grpcMockHandler) serve(srv any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	var reqFrame anyProto
	if err := stream.RecvMsg(&reqFrame); err != nil {
		return err
	}

	route, matched := matchGRPCRoute(h.routes, method)

	var resp *MockResponse
	switch {
	case matched && route.Dynamic != nil:
		dyn, err := route.Dynamic(MockRequest{Path: method, Body: reqFrame.data})
		if err != nil {
			emitWith(h.emit, "dynamic_error", map[string]string{"method": method, "error": err.Error()})
			return status.Error(codes.Internal, err.Error())
		}
		resp = dyn
	case matched:
		resp = route.Response
	case h.def != nil:
		resp = h.def
	default:
		emitWith(h.emit, "unmatched", map[string]string{"method": method})
		return status.Error(codes.Unimplemented, fmt.Sprintf("mock: no route for %s", method))
	}

	if resp == nil {
		return status.Error(codes.Unimplemented, "mock: nil response")
	}

	// Status != 0 is treated as a gRPC error code (see grpc_error()).
	if resp.Status != 0 {
		emitWith(h.emit, "error", map[string]string{
			"method": method,
			"code":   fmt.Sprintf("%d", resp.Status),
		})
		return status.Error(codes.Code(resp.Status), string(resp.Body))
	}

	out := anyProto{data: resp.Body}
	if err := stream.SendMsg(&out); err != nil {
		return err
	}

	emitWith(h.emit, "ok", map[string]string{
		"method":    method,
		"resp_size": fmt.Sprintf("%d", len(resp.Body)),
	})
	return nil
}

// matchGRPCRoute matches by exact method path with optional tail glob:
//
//	"/flags.v1.Flags/Get"    — exact
//	"/flags.v1.Flags/*"      — any method on the service
//	"/**"                    — catch-all
func matchGRPCRoute(routes []MockRoute, method string) (MockRoute, bool) {
	for _, r := range routes {
		if grpcMethodMatch(r.Pattern, method) {
			return r, true
		}
	}
	return MockRoute{}, false
}

func grpcMethodMatch(pattern, method string) bool {
	if pattern == method {
		return true
	}
	if pattern == "/**" {
		return true
	}
	// Trailing /* — wildcard method on a specific service.
	if len(pattern) > 2 && pattern[len(pattern)-2:] == "/*" {
		prefix := pattern[:len(pattern)-1] // keep the slash
		return len(method) > len(prefix) && method[:len(prefix)] == prefix
	}
	return false
}

// anyProto is a codec-neutral message type that carries raw protobuf-
// encoded bytes through grpc.ServerStream.RecvMsg/SendMsg without
// requiring a registered message type. It satisfies the proto.Message
// contract that grpc-go's default codec checks for.
type anyProto struct {
	data []byte
}

// Reset / String / ProtoMessage satisfy the proto.Message interface so
// grpc-go's "proto" codec accepts our value in SendMsg/RecvMsg.
func (a *anyProto) Reset()        { a.data = nil }
func (a *anyProto) String() string { return fmt.Sprintf("anyProto(%d bytes)", len(a.data)) }
func (a *anyProto) ProtoMessage() {}

// Marshal / Unmarshal satisfy grpc-go's special-case path for raw bytes
// messages. grpc-go checks for (Marshal() ([]byte, error)) before falling
// back to the proto codec, so carrying bytes through as-is works.
func (a *anyProto) Marshal() ([]byte, error) { return a.data, nil }
func (a *anyProto) Unmarshal(b []byte) error {
	a.data = append(a.data[:0], b...)
	return nil
}

// JSONToGRPCStruct converts JSON bytes into a protobuf-encoded
// google.protobuf.Struct. Used by the grpc_response() builtin so specs
// can return gRPC messages as ordinary Starlark dicts — the wire-level
// encoding is the well-known Struct type that reflection-based clients
// decode naturally.
func JSONToGRPCStruct(jsonBytes []byte) ([]byte, error) {
	var tmp map[string]any
	if err := json.Unmarshal(jsonBytes, &tmp); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	s, err := structpb.NewStruct(tmp)
	if err != nil {
		return nil, fmt.Errorf("build struct: %w", err)
	}
	return proto.Marshal(s)
}

// GRPCCodeByName returns the numeric gRPC status code for the given
// canonical name (UPPERCASE with underscores, e.g. "UNAVAILABLE").
// Returns (0, false) if the name is not recognized.
func GRPCCodeByName(name string) (int, bool) {
	codes := map[string]int{
		"OK":                  0,
		"CANCELLED":           1,
		"UNKNOWN":             2,
		"INVALID_ARGUMENT":    3,
		"DEADLINE_EXCEEDED":   4,
		"NOT_FOUND":           5,
		"ALREADY_EXISTS":      6,
		"PERMISSION_DENIED":   7,
		"RESOURCE_EXHAUSTED":  8,
		"FAILED_PRECONDITION": 9,
		"ABORTED":             10,
		"OUT_OF_RANGE":        11,
		"UNIMPLEMENTED":       12,
		"INTERNAL":            13,
		"UNAVAILABLE":         14,
		"DATA_LOSS":           15,
		"UNAUTHENTICATED":     16,
	}
	c, ok := codes[name]
	return c, ok
}
