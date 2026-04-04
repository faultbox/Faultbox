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
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/reflect/protodesc"
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
	return TCPHealthcheck(ctx, addr, timeout)
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

// rawInvoke calls a gRPC method using raw bytes (works without proto descriptors).
func (p *grpcProtocol) rawInvoke(ctx context.Context, conn *grpc.ClientConn, method, body string, start time.Time) (*StepResult, error) {
	// For raw invocation, we need the proto descriptor to marshal/unmarshal.
	// Without it, we can only send/receive raw bytes.
	// This is a simplified implementation — full reflection-based invoke
	// would discover descriptors and use dynamicpb.

	var reqBytes []byte
	if body != "{}" && body != "" {
		// Try to interpret body as JSON and convert to raw bytes.
		// For a real implementation, this would use protojson with the descriptor.
		reqBytes = []byte(body)
	}

	var respBytes []byte
	err := conn.Invoke(ctx, method, reqBytes, &respBytes)
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
	_ = protojson.MarshalOptions{}
	_ proto.Message          = (*dynamicpb.Message)(nil)
	_ = protodesc.NewFile
	_ = (*descriptorpb.FileDescriptorProto)(nil)
)
