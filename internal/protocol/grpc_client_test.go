package protocol

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// buildOrderDescriptorSet synthesizes a FileDescriptorSet for:
//
//	syntax = "proto3";
//	package orders.v1;
//	message GetOrderRequest { int64 order_id = 1; string include_items = 2; }
//	message Order { int64 id = 1; string eta = 2; }
//	message ListOrdersRequest { string status = 1; }
//	message OrderList { }
//	service OrderService {
//	  rpc GetOrder (GetOrderRequest) returns (Order);
//	  rpc ListOrdersV2 (ListOrdersRequest) returns (OrderList);
//	  rpc StreamOrders (ListOrdersRequest) returns (stream Order);   // skipped: streaming
//	}
func buildOrderDescriptorSet() *descriptorpb.FileDescriptorSet {
	syntax := "proto3"
	pkg := "orders.v1"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("orders/v1/orders.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("GetOrderRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("order_id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
					msgField("include_items", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
			{
				Name: proto.String("Order"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
					msgField("eta", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
			{
				Name: proto.String("ListOrdersRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("status", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
			{Name: proto.String("OrderList")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("OrderService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("GetOrder"),
						InputType:  proto.String(".orders.v1.GetOrderRequest"),
						OutputType: proto.String(".orders.v1.Order"),
					},
					{
						Name:       proto.String("ListOrdersV2"),
						InputType:  proto.String(".orders.v1.ListOrdersRequest"),
						OutputType: proto.String(".orders.v1.OrderList"),
					},
					{
						Name:            proto.String("StreamOrders"),
						InputType:       proto.String(".orders.v1.ListOrdersRequest"),
						OutputType:      proto.String(".orders.v1.Order"),
						ServerStreaming: proto.Bool(true),
					},
				},
			},
		},
	}
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
}

func loadGRPCOrdersTable(t *testing.T) *OperationTable {
	t.Helper()
	path := writeFds(t, buildOrderDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}
	table, err := BuildGRPCOperations(files, path, "", nil)
	if err != nil {
		t.Fatalf("BuildGRPCOperations: %v", err)
	}
	return table
}

func TestBuildGRPCOperations_NamesAndContract(t *testing.T) {
	table := loadGRPCOrdersTable(t)

	// StreamOrders is dropped: v1 is unary-only, and one streaming method
	// must not make the whole descriptor set unusable.
	want := []string{"get_order", "list_orders_v2"}
	got := table.Names()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("operation names = %v, want %v", got, want)
	}

	if table.Contract.Kind != ContractGRPC {
		t.Errorf("contract kind = %q, want %q", table.Contract.Kind, ContractGRPC)
	}
	if table.Contract.Version != "orders.v1.OrderService" {
		t.Errorf("contract version = %q, want the service FQN", table.Contract.Version)
	}

	op, ok := table.Lookup("get_order")
	if !ok {
		t.Fatal("get_order not found")
	}
	if op.FullMethod != "/orders.v1.OrderService/GetOrder" {
		t.Errorf("full method = %q", op.FullMethod)
	}
	if op.Wire() != "/orders.v1.OrderService/GetOrder" {
		t.Errorf("Wire() = %q", op.Wire())
	}

	// Request fields become kwargs, sorted by canonical name. proto3 has
	// no required fields, so none is marked required.
	if len(op.Params) != 2 {
		t.Fatalf("params = %+v, want 2", op.Params)
	}
	if op.Params[0].Name != "include_items" || op.Params[1].Name != "order_id" {
		t.Errorf("param names = %q, %q; want include_items, order_id", op.Params[0].Name, op.Params[1].Name)
	}
	for _, p := range op.Params {
		if p.In != ParamField {
			t.Errorf("param %q location = %q, want %q", p.Name, p.In, ParamField)
		}
		if p.Required {
			t.Errorf("param %q marked required; proto3 fields never are", p.Name)
		}
	}
}

func TestBuildGRPCOperations_LookupByContractName(t *testing.T) {
	table := loadGRPCOrdersTable(t)
	op, ok := table.LookupContractName("/orders.v1.OrderService/GetOrder")
	if !ok {
		t.Fatal("LookupContractName by full method path failed")
	}
	if op.Name != "get_order" {
		t.Errorf("resolved to %q, want get_order", op.Name)
	}
}

func TestBuildGRPCOperations_MultiServiceRequiresSelection(t *testing.T) {
	set := buildOrderDescriptorSet()
	// A second service in the same file forces an explicit choice.
	set.File[0].Service = append(set.File[0].Service, &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("BillingService"),
		Method: []*descriptorpb.MethodDescriptorProto{{
			Name:       proto.String("Charge"),
			InputType:  proto.String(".orders.v1.GetOrderRequest"),
			OutputType: proto.String(".orders.v1.Order"),
		}},
	})
	path := writeFds(t, set)
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	_, err = BuildGRPCOperations(files, path, "", nil)
	if err == nil {
		t.Fatal("expected an error when the set declares multiple services")
	}
	for _, want := range []string{"grpc_service=", "orders.v1.BillingService", "orders.v1.OrderService"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}

	// Naming one resolves it.
	table, err := BuildGRPCOperations(files, path, "orders.v1.BillingService", nil)
	if err != nil {
		t.Fatalf("BuildGRPCOperations with grpc_service=: %v", err)
	}
	if names := table.Names(); len(names) != 1 || names[0] != "charge" {
		t.Errorf("names = %v, want [charge]", names)
	}

	// An unknown service name lists the candidates.
	_, err = BuildGRPCOperations(files, path, "orders.v1.NoSuchService", nil)
	if err == nil || !strings.Contains(err.Error(), "available:") {
		t.Errorf("expected an error listing available services, got: %v", err)
	}
}

func TestBuildGRPCOperations_Rename(t *testing.T) {
	path := writeFds(t, buildOrderDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	// Rename accepts the bare method name...
	table, err := BuildGRPCOperations(files, path, "", map[string]string{"GetOrder": "fetch_order"})
	if err != nil {
		t.Fatalf("BuildGRPCOperations: %v", err)
	}
	if _, ok := table.Lookup("fetch_order"); !ok {
		t.Errorf("rename by method name failed; names = %v", table.Names())
	}

	// ...and the full wire path.
	table, err = BuildGRPCOperations(files, path, "",
		map[string]string{"/orders.v1.OrderService/GetOrder": "fetch_order"})
	if err != nil {
		t.Fatalf("BuildGRPCOperations: %v", err)
	}
	if _, ok := table.Lookup("fetch_order"); !ok {
		t.Errorf("rename by full path failed; names = %v", table.Names())
	}

	// An unused rename key is an error rather than a silent no-op.
	_, err = BuildGRPCOperations(files, path, "", map[string]string{"NoSuchMethod": "x"})
	if err == nil || !strings.Contains(err.Error(), "NoSuchMethod") {
		t.Errorf("expected an unused-rename error, got: %v", err)
	}
}

func TestBuildGRPCRequest(t *testing.T) {
	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")

	req, err := table.BuildGRPCRequest(op, map[string]any{
		"order_id":      int64(42),
		"include_items": "true",
	})
	if err != nil {
		t.Fatalf("BuildGRPCRequest: %v", err)
	}
	if req.FullMethod != "/orders.v1.OrderService/GetOrder" {
		t.Errorf("full method = %q", req.FullMethod)
	}

	// The message must round-trip through the wire format as the real type.
	wire, err := proto.Marshal(req.Message)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if len(wire) == 0 {
		t.Fatal("encoded request is empty")
	}

	var decoded map[string]any
	if err := json.Unmarshal(req.JSON, &decoded); err != nil {
		t.Fatalf("request JSON is not valid: %v", err)
	}
	if decoded["order_id"] != float64(42) {
		t.Errorf("request JSON order_id = %v, want 42", decoded["order_id"])
	}
}

func TestBuildGRPCRequest_Errors(t *testing.T) {
	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")

	t.Run("unknown kwarg suggests the nearest field", func(t *testing.T) {
		_, err := table.BuildGRPCRequest(op, map[string]any{"order_ids": int64(1)})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), `did you mean "order_id"`) {
			t.Errorf("expected a field suggestion, got: %v", err)
		}
	})

	t.Run("body= is rejected for gRPC", func(t *testing.T) {
		_, err := table.BuildGRPCRequest(op, map[string]any{"body": "{}"})
		if err == nil || !strings.Contains(err.Error(), "not body=") {
			t.Errorf("expected a body= rejection, got: %v", err)
		}
	})

	t.Run("wrong field type surfaces the message type", func(t *testing.T) {
		_, err := table.BuildGRPCRequest(op, map[string]any{"order_id": "not-a-number"})
		if err == nil {
			t.Fatal("expected a type error")
		}
		if !strings.Contains(err.Error(), "orders.v1.GetOrderRequest") {
			t.Errorf("error should name the request message type, got: %v", err)
		}
	})
}

// startTypedMock spins up the RFC-023 typed gRPC mock on a loopback port
// and returns its address. Using the real mock (rather than a bespoke test
// server) exercises the full contract loop: the same descriptor set encodes
// the response on the mock side and decodes it on the client side.
func startTypedMock(t *testing.T, routes []MockRoute) string {
	t.Helper()
	path := writeFds(t, buildOrderDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p := &grpcProtocol{}
	done := make(chan error, 1)
	go func() {
		done <- p.ServeMock(ctx, addr, MockSpec{Routes: routes, Descriptors: files}, nil)
	}()

	// Wait for the listener to accept before returning.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mock did not start on %s", addr)
	return ""
}

func TestInvokeGRPCUnary_TypedRoundTrip(t *testing.T) {
	addr := startTypedMock(t, []MockRoute{{
		Pattern:  "/orders.v1.OrderService/GetOrder",
		Response: &MockResponse{Body: []byte(`{"id": 1001, "eta": "12m"}`)},
	}})

	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")
	req, err := table.BuildGRPCRequest(op, map[string]any{"order_id": int64(1001)})
	if err != nil {
		t.Fatalf("BuildGRPCRequest: %v", err)
	}

	resp, err := InvokeGRPCUnary(context.Background(), addr, op, req, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("InvokeGRPCUnary: %v", err)
	}
	if resp.Code != codes.OK {
		t.Fatalf("code = %s (%s), want OK", resp.CodeName, resp.Message)
	}

	var decoded map[string]any
	if err := json.Unmarshal(resp.JSON, &decoded); err != nil {
		t.Fatalf("response JSON invalid: %v (raw %s)", err, resp.JSON)
	}
	// protojson renders int64 as a string and applies lowerCamelCase to
	// field names — both are protojson's documented behaviour, and the
	// Starlark layer sees exactly this shape on resp.data.
	if decoded["id"] != "1001" && decoded["id"] != float64(1001) {
		t.Errorf("response id = %v, want 1001", decoded["id"])
	}
	if decoded["eta"] != "12m" {
		t.Errorf("response eta missing from %v", decoded)
	}

	if err := table.ValidateGRPCResponse(op, resp); err != nil {
		t.Errorf("expected a conforming response, got: %v", err)
	}
}

func TestInvokeGRPCUnary_StatusError(t *testing.T) {
	// The mock turns a non-zero Status into a gRPC status code.
	addr := startTypedMock(t, []MockRoute{{
		Pattern:  "/orders.v1.OrderService/GetOrder",
		Response: &MockResponse{Status: int(codes.Unavailable), Body: []byte("orders down")},
	}})

	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")
	req, err := table.BuildGRPCRequest(op, map[string]any{"order_id": int64(1)})
	if err != nil {
		t.Fatalf("BuildGRPCRequest: %v", err)
	}

	resp, err := InvokeGRPCUnary(context.Background(), addr, op, req, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("InvokeGRPCUnary returned a transport error: %v", err)
	}
	if resp.Code != codes.Unavailable {
		t.Errorf("code = %s, want Unavailable", resp.CodeName)
	}
	if resp.CodeName != "Unavailable" {
		t.Errorf("code name = %q, want Unavailable", resp.CodeName)
	}
	if !strings.Contains(resp.Message, "orders down") {
		t.Errorf("status message = %q, want the mock's message", resp.Message)
	}

	// A non-OK status is an outcome, not a conformance failure — the test's
	// own assertions decide whether UNAVAILABLE was expected.
	if err := table.ValidateGRPCResponse(op, resp); err != nil {
		t.Errorf("a status error must not register as a contract violation: %v", err)
	}
}

func TestInvokeGRPCUnary_UnroutedMethodIsUnimplemented(t *testing.T) {
	addr := startTypedMock(t, nil)

	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")
	req, err := table.BuildGRPCRequest(op, map[string]any{"order_id": int64(1)})
	if err != nil {
		t.Fatalf("BuildGRPCRequest: %v", err)
	}

	resp, err := InvokeGRPCUnary(context.Background(), addr, op, req, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("InvokeGRPCUnary: %v", err)
	}
	if resp.Code != codes.Unimplemented {
		t.Errorf("code = %s, want Unimplemented", resp.CodeName)
	}
}

func TestInvokeGRPCUnary_ConnectionRefused(t *testing.T) {
	// Reserve and immediately release a port so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	table := loadGRPCOrdersTable(t)
	op, _ := table.Lookup("get_order")
	req, err := table.BuildGRPCRequest(op, map[string]any{"order_id": int64(1)})
	if err != nil {
		t.Fatalf("BuildGRPCRequest: %v", err)
	}

	resp, err := InvokeGRPCUnary(context.Background(), addr, op, req, nil, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("InvokeGRPCUnary should report transport failure as a status, not an error: %v", err)
	}
	if resp.Code == codes.OK {
		t.Error("expected a non-OK status when nothing is listening")
	}
	if resp.DurationMs < 0 {
		t.Errorf("duration = %d, want a non-negative measurement", resp.DurationMs)
	}
}
