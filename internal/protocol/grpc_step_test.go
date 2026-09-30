package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// This server deliberately has no reflection service and accepts only real
// protobuf requests. Its checks catch accidental JSON-on-the-wire regressions.
func TestGRPCStepTypedWithoutReflection(t *testing.T) {
	path := writeFds(t, buildOrderDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatal(err)
	}
	input, output, err := ResolveMethod(files, "/orders.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	addr := grpcStepServer(t, func(_ any, stream grpc.ServerStream) error {
		calls.Add(1)
		method, _ := grpc.MethodFromServerStream(stream)
		if method != "/orders.v1.OrderService/GetOrder" {
			return status.Error(codes.Unimplemented, "no reflection")
		}
		var wire []byte
		if err := stream.RecvMsg(&wire); err != nil {
			return err
		}
		req := dynamicpb.NewMessage(input)
		if err := proto.Unmarshal(wire, req); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if req.Get(input.Fields().ByName("order_id")).Int() != 9007199254740993 {
			return status.Error(codes.InvalidArgument, "order id lost precision")
		}
		md, _ := metadata.FromIncomingContext(stream.Context())
		if strings.Join(md.Get("x-tags"), ",") != "first,second" {
			return status.Error(codes.Unauthenticated, "missing metadata")
		}
		if err := stream.SendHeader(metadata.Pairs("x-server", "bidding")); err != nil {
			return err
		}
		resp := dynamicpb.NewMessage(output)
		resp.Set(output.Fields().ByName("id"), protoreflect.ValueOfInt64(9007199254740993))
		resp.Set(output.Fields().ByName("eta"), protoreflect.ValueOfString("готово"))
		out, err := proto.Marshal(resp)
		if err != nil {
			return err
		}
		return stream.SendMsg(out)
	})
	for _, body := range []any{map[string]any{"order_id": int64(9007199254740993)}, `{"order_id":"9007199254740993"}`} {
		result, err := (&grpcProtocol{}).ExecuteStep(context.Background(), addr, "call", map[string]any{
			"method": "/orders.v1.OrderService/GetOrder", "descriptors": path, "body": body,
			"metadata": map[string]any{"X-Tags": []any{"first", "second"}}, "timeout": "1s",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Success || result.StatusCode != 0 {
			t.Fatalf("result = %+v", result)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(result.Body), &data); err != nil {
			t.Fatal(err)
		}
		if data["id"] != "9007199254740993" || data["eta"] != "готово" {
			t.Fatalf("data = %v", data)
		}
		if strings.Join(result.Headers["x-server"], ",") != "bidding" {
			t.Fatalf("headers = %v", result.Headers)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d; reflection must not be attempted", calls.Load())
	}
}

func grpcStepServer(t *testing.T, handler grpc.StreamHandler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(rawBytesCodec{}), grpc.UnknownServiceHandler(handler))
	t.Cleanup(srv.Stop)
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String()
}

func TestGRPCStepStatusDeadlineAndRawBytes(t *testing.T) {
	addr := grpcStepServer(t, func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		switch method {
		case "/test.Service/Fail":
			return status.Error(codes.NotFound, "order missing")
		case "/test.Service/Wait":
			<-stream.Context().Done()
			return status.FromContextError(stream.Context().Err()).Err()
		}
		var wire []byte
		if err := stream.RecvMsg(&wire); err != nil {
			return err
		}
		return stream.SendMsg(wire)
	})
	p := &grpcProtocol{}
	result, err := p.ExecuteStep(context.Background(), addr, "call", map[string]any{"method": "/test.Service/Fail"})
	if err != nil || result.Success || result.StatusCode != int(codes.NotFound) || !strings.Contains(result.Error, "order missing") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	start := time.Now()
	result, err = p.ExecuteStep(context.Background(), addr, "call", map[string]any{"method": "/test.Service/Wait", "timeout": "30ms"})
	if err != nil || result.Success || result.StatusCode != int(codes.DeadlineExceeded) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = p.ExecuteStep(ctx, addr, "call", map[string]any{"method": "/test.Service/Wait"})
	if err != nil || result.StatusCode != int(codes.Canceled) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, args := range []map[string]any{
		{"method": "/test.Service/Echo", "body": "legacy payload"},
		{"method": "/test.Service/Echo", "body": "{}"},
		{"method": "/test.Service/Echo", "body_base64": base64.StdEncoding.EncodeToString([]byte{0, 255, 128, 10})},
	} {
		result, err := p.ExecuteStep(context.Background(), addr, "call", args)
		if err != nil || !result.Success {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var data map[string]string
		if err := json.Unmarshal([]byte(result.Body), &data); err != nil {
			t.Fatal(err)
		}
		expected := args["body_base64"]
		if expected == nil {
			body := args["body"].(string)
			if body == "{}" {
				body = ""
			}
			expected = base64.StdEncoding.EncodeToString([]byte(body))
		}
		if data["raw_base64"] != expected {
			t.Fatalf("data=%v expected=%v", data, expected)
		}
	}
}

func TestGRPCStepRejectsInvalidArguments(t *testing.T) {
	path := writeFds(t, buildOrderDescriptorSet())
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown field", map[string]any{"descriptors": path, "body": `{"orderid":42}`}, "unknown field"},
		{"malformed json", map[string]any{"descriptors": path, "body": `{`}, "encode"},
		{"missing descriptor", map[string]any{"descriptors": "/does/not/exist"}, "read descriptor"},
		{"stream", map[string]any{"descriptors": path, "method": "/orders.v1.OrderService/StreamOrders"}, "unary"},
		{"dict requires schema", map[string]any{"body": map[string]any{}}, "requires descriptors"},
		{"raw ambiguity", map[string]any{"body": "", "body_base64": ""}, "either body"},
		{"typed raw ambiguity", map[string]any{"descriptors": path, "body_base64": ""}, "cannot be combined"},
		{"base64", map[string]any{"body_base64": "!"}, "body_base64"},
		{"timeout", map[string]any{"timeout": "0s"}, "positive duration"},
		{"metadata", map[string]any{"metadata": map[string]any{"x-id": []any{1}}}, "must contain strings"},
		{"method", map[string]any{"method": "orders.v1.OrderService/GetOrder"}, "requires method"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.args["method"]; !ok {
				tc.args["method"] = "/orders.v1.OrderService/GetOrder"
			}
			_, err := (&grpcProtocol{}).ExecuteStep(context.Background(), "127.0.0.1:1", "call", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}
