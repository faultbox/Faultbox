package protocol

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildCityDescriptorSet synthesizes a minimal FileDescriptorSet mimicking
// what protoc would emit for:
//
//	syntax = "proto3";
//	package test.geo;
//	message City {
//	  int64 id = 1;
//	  string name = 2;
//	  string country = 3;
//	  string currency = 4;
//	}
//	message GetCityRequest { int64 id = 1; }
//	service GeoService {
//	  rpc GetCity (GetCityRequest) returns (City);
//	}
//
// Used by the tests below to exercise LoadDescriptorSet + ResolveMethod +
// JSONToTypedMessage end-to-end without requiring a protoc invocation in
// the test suite.
func buildCityDescriptorSet() *descriptorpb.FileDescriptorSet {
	syntax := "proto3"
	pkg := "test.geo"
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("test/geo.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("City"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
					msgField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
					msgField("country", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
					msgField("currency", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				},
			},
			{
				Name: proto.String("GetCityRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("GeoService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("GetCity"),
						InputType:  proto.String(".test.geo.GetCityRequest"),
						OutputType: proto.String(".test.geo.City"),
					},
				},
			},
		},
	}
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
}

func msgField(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Type:   &typ,
		Label:  &label,
	}
}

// writeFds serializes a FileDescriptorSet to a temp file and returns the
// path so LoadDescriptorSet can read it — mirrors the `.pb` output shape
// that protoc would produce.
func writeFds(t *testing.T, set *descriptorpb.FileDescriptorSet) string {
	t.Helper()
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.pb")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write pb: %v", err)
	}
	return path
}

// TestLoadDescriptorSet_RegistersCustomerAndWellKnownTypes verifies that
// LoadDescriptorSet produces a registry containing (a) every message in
// the customer's set and (b) the standard google.protobuf.* well-known
// types that downstream customer files may import.
func TestLoadDescriptorSet_RegistersCustomerAndWellKnownTypes(t *testing.T) {
	path := writeFds(t, buildCityDescriptorSet())

	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	// Customer messages are discoverable.
	for _, name := range []string{"test.geo.City", "test.geo.GetCityRequest", "test.geo.GeoService"} {
		if _, err := files.FindDescriptorByName(protoreflect.FullName(name)); err != nil {
			t.Errorf("customer descriptor %q not found: %v", name, err)
		}
	}

	// Standard WKTs are pre-registered (so a customer file importing
	// google/protobuf/timestamp.proto resolves without --include_imports).
	for _, name := range []string{"google.protobuf.Timestamp", "google.protobuf.Empty", "google.protobuf.Any", "google.protobuf.Struct"} {
		if _, err := files.FindDescriptorByName(protoreflect.FullName(name)); err != nil {
			t.Errorf("well-known type %q missing from registry: %v", name, err)
		}
	}
}

// TestResolveMethod_HappyPath looks up a method on the synthetic GeoService
// and confirms the input/output message descriptors come back correctly —
// the exact lookup the typed-mock handler will do on every request.
func TestResolveMethod_HappyPath(t *testing.T) {
	path := writeFds(t, buildCityDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	in, out, err := ResolveMethod(files, "/test.geo.GeoService/GetCity")
	if err != nil {
		t.Fatalf("ResolveMethod: %v", err)
	}
	if in.FullName() != "test.geo.GetCityRequest" {
		t.Errorf("input type = %q, want test.geo.GetCityRequest", in.FullName())
	}
	if out.FullName() != "test.geo.City" {
		t.Errorf("output type = %q, want test.geo.City", out.FullName())
	}
}

// TestResolveMethod_ErrorCases covers the three failure classes a spec
// author can hit: malformed path, service not in set, method not on service.
// Each returns a distinct, helpful error message.
func TestResolveMethod_ErrorCases(t *testing.T) {
	path := writeFds(t, buildCityDescriptorSet())
	files, _ := LoadDescriptorSet(path)

	cases := []struct {
		name        string
		method      string
		wantErrFrag string
	}{
		{"malformed no slash", "foo", "invalid gRPC method path"},
		{"malformed trailing slash", "/test.geo.GeoService/", "invalid gRPC method path"},
		{"service missing", "/test.missing.Service/Method", "service \"test.missing.Service\" not found"},
		{"method missing", "/test.geo.GeoService/NoSuch", "method \"NoSuch\" not found on service \"test.geo.GeoService\" (available: GetCity)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ResolveMethod(files, tc.method)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("error = %q, want fragment %q", err.Error(), tc.wantErrFrag)
			}
		})
	}
}

// TestJSONToTypedMessage_RoundTrip is the load-bearing test for the
// whole encoder: given a typed message descriptor (City) and a
// Starlark-style JSON response dict, it produces wire bytes that a
// real compiled-stub client would decode back into the same values.
// Simulates the client side with dynamicpb.Unmarshal — the customer's
// generated *.pb.go code does the equivalent with their compiled
// message struct.
func TestJSONToTypedMessage_RoundTrip(t *testing.T) {
	path := writeFds(t, buildCityDescriptorSet())
	files, err := LoadDescriptorSet(path)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}
	desc, err := files.FindDescriptorByName("test.geo.City")
	if err != nil {
		t.Fatalf("find City: %v", err)
	}
	cityMd := desc.(protoreflect.MessageDescriptor)

	jsonResp := []byte(`{"id": 42, "name": "Almaty", "country": "KZ", "currency": "KZT"}`)

	wire, err := JSONToTypedMessage(files, cityMd, jsonResp)
	if err != nil {
		t.Fatalf("JSONToTypedMessage: %v", err)
	}
	if len(wire) == 0 {
		t.Fatal("wire bytes empty")
	}

	// Decode on the "client side" using dynamicpb — a compiled proto stub
	// would decode equivalently. If the encoder got any field wrong,
	// Unmarshal would fail or produce zero values.
	got := dynamicpb.NewMessage(cityMd)
	if err := proto.Unmarshal(wire, got); err != nil {
		t.Fatalf("decode wire: %v", err)
	}

	idF := cityMd.Fields().ByName("id")
	nameF := cityMd.Fields().ByName("name")
	countryF := cityMd.Fields().ByName("country")
	currencyF := cityMd.Fields().ByName("currency")

	if v := got.Get(idF).Int(); v != 42 {
		t.Errorf("id = %d, want 42", v)
	}
	if v := got.Get(nameF).String(); v != "Almaty" {
		t.Errorf("name = %q, want Almaty", v)
	}
	if v := got.Get(countryF).String(); v != "KZ" {
		t.Errorf("country = %q, want KZ", v)
	}
	if v := got.Get(currencyF).String(); v != "KZT" {
		t.Errorf("currency = %q, want KZT", v)
	}
}

// TestJSONToTypedMessage_UnknownFieldSurfaces verifies that a typo in
// the spec's response dict fails with an error that names the offending
// field — so spec authors see "unknown field cityid" rather than a
// downstream DataLoss on the wire.
func TestJSONToTypedMessage_UnknownFieldSurfaces(t *testing.T) {
	path := writeFds(t, buildCityDescriptorSet())
	files, _ := LoadDescriptorSet(path)
	desc, _ := files.FindDescriptorByName("test.geo.City")
	cityMd := desc.(protoreflect.MessageDescriptor)

	// "cityid" instead of "id" — common copy-paste typo.
	bad := []byte(`{"cityid": 42, "name": "Almaty"}`)
	_, err := JSONToTypedMessage(files, cityMd, bad)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !contains(err.Error(), "cityid") {
		t.Errorf("error should name the offending field, got %q", err.Error())
	}
	if !contains(err.Error(), "test.geo.City") {
		t.Errorf("error should name the target message, got %q", err.Error())
	}
}

// TestLoadDescriptorSet_IsolationBetweenMocks confirms two descriptor sets
// loaded from different paths don't share state — one mock's types can't
// leak into another mock's registry. Important for multi-mock topologies
// (truck-api Phase 1 shape: 8 upstream mocks each with their own proto
// packages).
func TestLoadDescriptorSet_IsolationBetweenMocks(t *testing.T) {
	path1 := writeFds(t, buildCityDescriptorSet())
	// Second set with a different message — "Order" instead of "City".
	syntax := "proto3"
	pkg := "test.order"
	order := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("test/order.proto"),
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Order"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				},
			},
		},
	}
	path2 := writeFds(t, &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{order}})

	files1, err := LoadDescriptorSet(path1)
	if err != nil {
		t.Fatalf("load set 1: %v", err)
	}
	files2, err := LoadDescriptorSet(path2)
	if err != nil {
		t.Fatalf("load set 2: %v", err)
	}

	// Each registry sees only its own types (plus WKTs).
	if _, err := files1.FindDescriptorByName("test.order.Order"); err == nil {
		t.Error("set 1 should NOT contain test.order.Order")
	}
	if _, err := files2.FindDescriptorByName("test.geo.City"); err == nil {
		t.Error("set 2 should NOT contain test.geo.City")
	}
	if _, err := files1.FindDescriptorByName("test.geo.City"); err != nil {
		t.Errorf("set 1 lost its own City: %v", err)
	}
	if _, err := files2.FindDescriptorByName("test.order.Order"); err != nil {
		t.Errorf("set 2 lost its own Order: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Guard protoregistry import so build tags don't strip it.
var _ = protoregistry.GlobalFiles
