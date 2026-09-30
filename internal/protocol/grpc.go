package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func init() { Register(&grpcProtocol{}) }

type grpcProtocol struct{}

func (p *grpcProtocol) Name() string      { return "grpc" }
func (p *grpcProtocol) Methods() []string { return []string{"call"} }
func (p *grpcProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return TCPHealthcheck(ctx, ParseAddr(addr).HostPort, timeout)
}

// ExecuteStep invokes a unary RPC. A descriptor set enables JSON/protobuf
// conversion without server reflection. Calls without descriptors preserve the
// original opaque-byte behavior; body_base64 makes that mode binary-safe.
func (p *grpcProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	if method != "call" {
		return nil, fmt.Errorf("unsupported grpc method %q (supported: call)", method)
	}
	rpcMethod, ok := kwargs["method"].(string)
	if !ok || !strings.HasPrefix(rpcMethod, "/") || len(strings.Split(rpcMethod, "/")) != 3 || strings.Contains(rpcMethod, "//") || strings.HasSuffix(rpcMethod, "/") {
		return nil, fmt.Errorf("grpc.call requires method='/package.Service/Method'")
	}
	descriptors := ""
	if value, present := kwargs["descriptors"]; present {
		descriptors, ok = value.(string)
		if !ok || descriptors == "" {
			return nil, fmt.Errorf("grpc.call descriptors must be a non-empty path string")
		}
	}
	start := time.Now()
	if value, present := kwargs["timeout"]; present {
		timeoutString, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("grpc.call timeout must be a duration string")
		}
		timeout, err := time.ParseDuration(timeoutString)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("grpc.call timeout must be a positive duration")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if value, present := kwargs["metadata"]; present {
		md, err := grpcStepMetadata(value)
		if err != nil {
			return nil, err
		}
		existing, _ := metadata.FromOutgoingContext(ctx)
		ctx = metadata.NewOutgoingContext(ctx, metadata.Join(existing, md))
	}

	var request, response any
	var decode func() ([]byte, error)
	var options []grpc.CallOption
	if descriptors != "" {
		if _, present := kwargs["body_base64"]; present {
			return nil, fmt.Errorf("grpc.call body_base64 cannot be combined with descriptors")
		}
		files, err := LoadDescriptorSet(descriptors)
		if err != nil {
			return nil, err
		}
		input, output, err := ResolveMethod(files, rpcMethod)
		if err != nil {
			return nil, err
		}
		descriptorName := protoreflect.FullName(strings.ReplaceAll(strings.TrimPrefix(rpcMethod, "/"), "/", "."))
		desc, err := files.FindDescriptorByName(descriptorName)
		if err != nil {
			return nil, err
		}
		rpc := desc.(protoreflect.MethodDescriptor)
		if rpc.IsStreamingClient() || rpc.IsStreamingServer() {
			return nil, fmt.Errorf("grpc.call supports unary methods only: %s", rpcMethod)
		}
		body := []byte("{}")
		if value, present := kwargs["body"]; present {
			switch v := value.(type) {
			case string:
				body = []byte(v)
			case map[string]any:
				body, err = json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("grpc.call encode body: %w", err)
				}
			default:
				return nil, fmt.Errorf("grpc.call typed body must be a dict or JSON string, got %T", value)
			}
		}
		req, resp := dynamicpb.NewMessage(input), dynamicpb.NewMessage(output)
		if err := (protojson.UnmarshalOptions{Resolver: typesResolver{files: files}}).Unmarshal(body, req); err != nil {
			return nil, fmt.Errorf("grpc.call encode %s as %s: %w", rpcMethod, input.FullName(), enrichProtoFieldError(err, input))
		}
		request, response = req, resp
		decode = func() ([]byte, error) {
			return (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true, Resolver: typesResolver{files: files}}).Marshal(resp)
		}
	} else {
		var req []byte
		var resp []byte
		if value, present := kwargs["body_base64"]; present {
			if _, present := kwargs["body"]; present {
				return nil, fmt.Errorf("grpc.call accepts either body or body_base64, not both")
			}
			encoded, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("grpc.call body_base64 must be a string")
			}
			var err error
			req, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("grpc.call body_base64: %w", err)
			}
		} else if value, present := kwargs["body"]; present {
			body, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("grpc.call dict body requires descriptors; raw body must be a string")
			}
			// Preserve legacy empty-request shorthand.
			if body != "{}" {
				req = []byte(body)
			}
		}
		request, response = req, &resp
		options = append(options, grpc.ForceCodec(rawBytesCodec{}), grpc.CallContentSubtype("proto"))
		decode = func() ([]byte, error) {
			return json.Marshal(map[string]any{"method": rpcMethod, "raw": string(resp), "raw_base64": base64.StdEncoding.EncodeToString(resp)})
		}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("grpc.call connect: %w", err)
	}
	defer conn.Close()
	var headers metadata.MD
	options = append(options, grpc.Header(&headers))
	invokeErr := conn.Invoke(ctx, rpcMethod, request, response, options...)
	result := &StepResult{Success: invokeErr == nil, StatusCode: int(status.Code(invokeErr)), Headers: headers, DurationMs: time.Since(start).Milliseconds()}
	if invokeErr != nil {
		result.Error = invokeErr.Error()
		return result, nil
	}
	body, err := decode()
	if err != nil {
		return nil, fmt.Errorf("grpc.call decode %s response: %w", rpcMethod, err)
	}
	result.Body = string(body)
	return result, nil
}

func grpcStepMetadata(value any) (metadata.MD, error) {
	values, ok := value.(map[string]any)
	if !ok {
		if stringsMap, supported := value.(map[string]string); supported {
			values = make(map[string]any, len(stringsMap))
			for key, value := range stringsMap {
				values[key] = value
			}
		} else {
			return nil, fmt.Errorf("grpc.call metadata must be a dict of strings or string lists")
		}
	}
	md := metadata.MD{}
	for key, value := range values {
		switch v := value.(type) {
		case string:
			md.Append(key, v)
		case []string:
			md.Append(key, v...)
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("grpc.call metadata[%q] must contain strings", key)
				}
				md.Append(key, s)
			}
		default:
			return nil, fmt.Errorf("grpc.call metadata[%q] must be a string or string list", key)
		}
	}
	return md, nil
}

// rawBytesCodec preserves opaque protobuf bytes for legacy calls.
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
