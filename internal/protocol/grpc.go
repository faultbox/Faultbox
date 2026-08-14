package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func init() {
	Register(&grpcProtocol{})
}

type grpcProtocol struct{}

func (p *grpcProtocol) Name() string { return "grpc" }

func (p *grpcProtocol) Methods() []string {
	return []string{"call"}
}

func (p *grpcProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return TCPHealthcheck(ctx, ParseAddr(addr).HostPort, timeout)
}

func (p *grpcProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	if method != "call" {
		return nil, fmt.Errorf("unsupported grpc method %q (supported: call)", method)
	}

	rpcMethod := getStringKwarg(kwargs, "method", "")
	if rpcMethod == "" {
		return nil, fmt.Errorf("grpc.call requires method= argument (e.g., '/package.Service/Method')")
	}
	body := getStringKwarg(kwargs, "body", "{}")

	start := time.Now()

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("connect: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer conn.Close()

	// Use reflection to discover the service and method descriptors.
	refClient := grpc_reflection_v1alpha.NewServerReflectionClient(conn)
	stream, err := refClient.ServerReflectionInfo(ctx)
	if err != nil {
		// Fallback: raw invoke without reflection (for services without reflection).
		return p.rawInvoke(ctx, conn, rpcMethod, body, start)
	}
	defer stream.CloseSend()

	// Try to resolve the method via reflection for proper marshaling.
	// On failure, fall back to raw invoke.
	_ = stream
	return p.rawInvoke(ctx, conn, rpcMethod, body, start)
}

// rawBytesCodec passes payloads through untouched.
//
// grpc-go's default codec requires a proto.Message, so handing it a
// []byte failed at the client before a single byte reached the wire:
//
//	rpc error: code = Internal desc = grpc: error while marshaling:
//	proto: failed to marshal, message is []uint8, want proto.Message
//
// which meant `grpc.call()` could not complete a round trip against any
// real server. Unit tests did not catch it because they never dialled one
// — the gap the protocol audit exists to close.
//
// Forcing this codec makes the request and response opaque bytes. It is
// deliberately not a full descriptor-based invoke: a JSON body is still
// sent verbatim rather than marshalled into protobuf, so `body=` only
// works for a method whose request is empty (the common health / ping
// shape) or when the caller supplies real wire bytes. Resolving
// descriptors via reflection and going through dynamicpb is the complete
// answer and is a larger piece of work.
type rawBytesCodec struct{}

func (rawBytesCodec) Name() string { return "faultbox-raw-bytes" }

func (rawBytesCodec) Marshal(v any) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case *[]byte:
		if b == nil {
			return nil, nil
		}
		return *b, nil
	default:
		return nil, fmt.Errorf("faultbox-raw-bytes: cannot marshal %T", v)
	}
}

func (rawBytesCodec) Unmarshal(data []byte, v any) error {
	out, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("faultbox-raw-bytes: cannot unmarshal into %T", v)
	}
	*out = append((*out)[:0], data...)
	return nil
}

// rawInvoke calls a gRPC method using raw bytes (works without proto descriptors).
func (p *grpcProtocol) rawInvoke(ctx context.Context, conn *grpc.ClientConn, method, body string, start time.Time) (*StepResult, error) {
	var reqBytes []byte
	if body != "{}" && body != "" {
		// Sent verbatim — see rawBytesCodec on what that does and does
		// not support.
		reqBytes = []byte(body)
	}

	var respBytes []byte
	err := conn.Invoke(ctx, method, reqBytes, &respBytes, grpc.ForceCodec(rawBytesCodec{}))
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	respJSON, _ := json.Marshal(map[string]any{
		"method": method,
		"raw":    string(respBytes),
	})
	return &StepResult{
		Body:       string(respJSON),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// Ensure imports are used (these will be needed for full reflection-based invoke).
var (
	_               = protojson.MarshalOptions{}
	_ proto.Message = (*dynamicpb.Message)(nil)
	_               = protodesc.NewFile
	_               = (*descriptorpb.FileDescriptorProto)(nil)
)
