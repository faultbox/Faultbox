package star

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	bsonpkg "go.mongodb.org/mongo-driver/v2/bson"
	mongodriver "go.mongodb.org/mongo-driver/v2/mongo"
	mongoopts "go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/net/http2"
	grpcdial "google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpccreds "google.golang.org/grpc/credentials"
	insecurecreds "google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	grpcstructpb "google.golang.org/protobuf/types/known/structpb"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// TestMockServiceSpecLoad verifies mock_service() parses and registers a
// service with correct routes + flags, without starting the runtime.
func TestMockServiceSpecLoad(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
auth = mock_service("auth",
    interface("http", "http", 18090),
    routes = {
        "GET /.well-known/jwks": json_response(status = 200, body = {"keys": []}),
        "GET /health":            status_only(204),
    },
    default = json_response(status = 404, body = {"error": "not found"}),
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	services := rt.Services()
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	svc := services[0]
	if !svc.IsMock() {
		t.Fatalf("expected service to be mock")
	}
	if svc.IsContainer() {
		t.Fatalf("mock must not report IsContainer")
	}
	if _, ok := svc.Interfaces["http"]; !ok {
		t.Fatalf("missing http interface: %v", svc.Interfaces)
	}
	routes := svc.Mock.Routes["http"]
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].Pattern != "GET /.well-known/jwks" {
		t.Fatalf("routes[0].Pattern = %q", routes[0].Pattern)
	}
	if svc.Mock.Default["http"] == nil {
		t.Fatal("default response missing")
	}
}

// TestMockServiceUnknownProtocol verifies the spec-load guard on protocols
// without a MockHandler implementation (e.g., postgres today).
func TestMockServiceUnknownProtocol(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = mock_service("db",
    interface("main", "postgres", 5432),
)
`)
	if err == nil {
		t.Fatal("expected spec load to fail for protocol without MockHandler")
	}
	if !strings.Contains(err.Error(), "does not support mock_service") {
		t.Fatalf("error mismatch: %v", err)
	}
}

// TestMockServiceEndToEnd stands up a mock HTTP service via the runtime,
// issues a real HTTP request against it, and verifies the response.
func TestMockServiceEndToEnd(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
auth = mock_service("auth",
    interface("http", "http", %d),
    routes = {
        "GET /.well-known/jwks": json_response(status = 200, body = {"keys": [{"kid": "test-1"}]}),
        "POST /token":           dynamic(lambda req: json_response(status = 200, body = {"query_user": req["query"].get("user", "anon")})),
    },
)

def test_auth_stub():
    pass
`, port)
	if err := rt.LoadString("mock_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}

	// Static JSON route.
	resp, err := client.Get("http://" + addr + "/.well-known/jwks")
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("jwks status = %d, want 200, body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"kid":"test-1"`) {
		t.Fatalf("jwks body = %q", string(body))
	}

	// Dynamic route (lambda in spec inspects request).
	resp, err = client.Post("http://"+addr+"/token?user=alice", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("token status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"query_user":"alice"`) {
		t.Fatalf("token body = %q", string(body))
	}

	// Unmatched → 404 (default fallback).
	resp, err = client.Get("http://" + addr + "/missing")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing status = %d, want 404", resp.StatusCode)
	}

	// Event log should have mock.<op> entries for every handled request.
	events := rt.events.Events()
	mockEvents := 0
	for _, e := range events {
		if strings.HasPrefix(e.Type, "mock.") {
			mockEvents++
		}
	}
	if mockEvents < 3 {
		t.Fatalf("expected >=3 mock.<op> events, got %d (events=%+v)", mockEvents, events)
	}
}

// TestMockServiceRestartClean verifies that startServices → stopServices
// frees the port so a subsequent test can bind it again. Protects against
// goroutine leaks from the mock handler.
func TestMockServiceRestartClean(t *testing.T) {
	port := freePort(t)

	for i := 0; i < 2; i++ {
		rt := New(testLogger())
		src := fmt.Sprintf(`
s = mock_service("s", interface("http", "http", %d),
    routes = {"GET /": status_only(200)})
`, port)
		if err := rt.LoadString("restart.star", src); err != nil {
			t.Fatalf("iter %d LoadString: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rt.startServices(ctx); err != nil {
			cancel()
			t.Fatalf("iter %d start: %v", i, err)
		}
		rt.stopServices()
		cancel()

		// Port should be re-bindable immediately.
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("iter %d rebind: %v", i, err)
		}
		ln.Close()
	}
}

// TestMockServiceHTTP2 verifies a mock_service declared with protocol http2
// stands up an h2c listener and serves the same route table format.
func TestMockServiceHTTP2(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
gw = mock_service("gw",
    interface("public", "http2", %d),
    routes = {
        "GET /healthz":    status_only(200),
        "POST /api/v1/**": json_response(status = 200, body = {"ok": True}),
    },
)

def test_h2_stub():
    pass
`, port)
	if err := rt.LoadString("h2_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	client := newTestH2CClient()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("healthz proto = %q, want HTTP/2.0", resp.Proto)
	}

	req, _ = http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/api/v1/orders/42", strings.NewReader(`{"x":1}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("api body = %q", string(body))
	}
}

// TestMockServiceTCP verifies a TCP mock_service stands up and responds to
// line-framed input according to bytes_response() routes.
func TestMockServiceTCP(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
legacy = mock_service("legacy",
    interface("main", "tcp", %d),
    routes = {
        "PING\n":    bytes_response(data = "PONG\n"),
        "VERSION\n": bytes_response(data = "2.0.0\n"),
    },
    default = bytes_response(data = "ERR\n"),
)

def test_tcp():
    pass
`, port)
	if err := rt.LoadString("tcp_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cases := []struct{ in, want string }{
		{"PING\n", "PONG\n"},
		{"VERSION\n", "2.0.0\n"},
		{"HUH\n", "ERR\n"},
	}
	for _, tc := range cases {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_, _ = conn.Write([]byte(tc.in))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Close()
		if string(buf[:n]) != tc.want {
			t.Errorf("send %q: got %q, want %q", tc.in, buf[:n], tc.want)
		}
	}
}

// TestMockServiceUDP verifies a UDP mock_service swallows datagrams by
// default and emits one event per datagram into the runtime event log.
func TestMockServiceUDP(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
statsd = mock_service("statsd",
    interface("main", "udp", %d),
)
`, port)
	if err := rt.LoadString("udp_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for i := 0; i < 5; i++ {
		_, _ = conn.Write([]byte(fmt.Sprintf("gauge.%d:%d|g", i, i)))
	}
	conn.Close()

	// Event log should have >=5 mock.recv entries after a brief wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, e := range rt.events.Events() {
			if e.Type == "mock.recv" {
				count++
			}
		}
		if count >= 5 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected >=5 mock.recv events; final events: %+v", rt.events.Events())
}

// TestMockServiceKafkaStdlib verifies that @faultbox/mocks/kafka.star's
// kafka.broker() stdlib constructor stands up a Kafka mock via kfake and a
// real kafka-go client can connect + list the seeded topics. This is the
// first end-to-end test of the @faultbox/mocks/ stdlib distribution path.
func TestMockServiceKafkaStdlib(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/kafka.star", "kafka")

bus = kafka.broker(
    name      = "bus",
    interface = interface("main", "kafka", %d),
    topics    = {"orders": [], "payments": []},
)
`, port)
	if err := rt.LoadString("kafka_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Wait for kfake's metadata endpoint to be responsive.
	deadline := time.Now().Add(5 * time.Second)
	var partitions []kafkago.Partition
	for time.Now().Before(deadline) {
		conn, err := kafkago.DialContext(ctx, "tcp", addr)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		partitions, err = conn.ReadPartitions()
		conn.Close()
		if err == nil && len(partitions) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	seen := make(map[string]bool)
	for _, p := range partitions {
		seen[p.Topic] = true
	}
	for _, want := range []string{"orders", "payments"} {
		if !seen[want] {
			t.Errorf("topic %q not found after stdlib kafka.broker(); seen=%+v", want, seen)
		}
	}
}

// TestMockServiceRedisStdlib verifies @faultbox/mocks/redis.star stands up
// a Redis mock with seeded state and a raw RESP client reads it.
func TestMockServiceRedisStdlib(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/redis.star", "redis")

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", %d),
    state     = {"greeting": "hello", "counter": "42"},
)
`, port)
	if err := rt.LoadString("redis_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Seeded values should be returned.
	_, _ = conn.Write([]byte("*2\r\n$3\r\nGET\r\n$8\r\ngreeting\r\n"))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "hello") {
		t.Errorf("GET greeting response = %q, want to contain 'hello'", buf[:n])
	}
}

// TestMockServiceGRPCStdlib verifies @faultbox/mocks/grpc.star stands up
// a typed gRPC mock backed by a FileDescriptorSet, and a compiled-stub-
// style client decodes responses as the customer's real message type
// (not google.protobuf.Struct). Regression guard for RFC-023 Phase 2.
func TestMockServiceGRPCStdlib(t *testing.T) {
	// Materialize a synthetic FileDescriptorSet matching the City / GeoService
	// shape used in grpc_typed_encoder_test.go. The spec will load this .pb
	// via descriptors= and encode responses against it.
	pbPath := writeTestDescriptorSet(t)

	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/grpc.star", "grpc")

geo = grpc.server(
    name        = "geo",
    interface   = interface("main", "grpc", %d),
    descriptors = %q,
    services    = {
        "/test.geo.GeoService/GetCity": {
            "response": {"id": 42, "name": "Almaty", "country": "KZ", "currency": "KZT"},
        },
    },
)
`, port, pbPath)
	if err := rt.LoadString("grpc_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	// Use grpc.NewClient + an anyProto-style receiver so we can decode
	// against the real descriptor without generated stubs — simulates what
	// a compiled *.pb.go client does internally.
	conn, err := grpcdial.NewClient(addr, grpcdial.WithTransportCredentials(insecurecreds.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	// Load the same .pb on the client side to get the City descriptor.
	files, err := protocol.LoadDescriptorSet(pbPath)
	if err != nil {
		t.Fatalf("LoadDescriptorSet on client side: %v", err)
	}
	cityDesc, _ := files.FindDescriptorByName("test.geo.City")
	cityMd := cityDesc.(protoreflect.MessageDescriptor)
	got := dynamicpb.NewMessage(cityMd)

	if err := conn.Invoke(context.Background(), "/test.geo.GeoService/GetCity",
		&grpcEmptyMsg{}, &grpcTypedRecv{msg: got}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if v := got.Get(cityMd.Fields().ByName("id")).Int(); v != 42 {
		t.Errorf("City.id = %d, want 42", v)
	}
	if v := got.Get(cityMd.Fields().ByName("name")).String(); v != "Almaty" {
		t.Errorf("City.name = %q, want Almaty", v)
	}
}

// TestMockServiceGRPCStdlib_ErrorRoute verifies grpc.server() with an
// "error" service spec maps through grpc.error() to the wire-level
// gRPC status code.
func TestMockServiceGRPCStdlib_ErrorRoute(t *testing.T) {
	pbPath := writeTestDescriptorSet(t)

	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/grpc.star", "grpc")

geo = grpc.server(
    name        = "geo",
    interface   = interface("main", "grpc", %d),
    descriptors = %q,
    services    = {
        "/test.geo.GeoService/GetCity": {
            "error": {"code": "PERMISSION_DENIED", "message": "admin only"},
        },
    },
)
`, port, pbPath)
	if err := rt.LoadString("grpc_err.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, _ := grpcdial.NewClient(addr, grpcdial.WithTransportCredentials(insecurecreds.NewCredentials()))
	defer conn.Close()

	var resp anyRecv
	err := conn.Invoke(context.Background(), "/test.geo.GeoService/GetCity",
		&grpcEmptyMsg{}, &resp)
	st, ok := grpcstatus.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %T: %v", err, err)
	}
	if st.Code() != grpccodes.PermissionDenied {
		t.Errorf("status code = %s, want PermissionDenied", st.Code())
	}
	if st.Message() != "admin only" {
		t.Errorf("status message = %q, want \"admin only\"", st.Message())
	}
}

// TestMockServiceGRPCStdlibShorthands verifies the v0.9.8 per-code
// helpers (grpc.unavailable, grpc.deadline_exceeded, …) produce the
// same wire-level status as grpc.error(code="…"). The shorthand is
// what customers reach for when hand-writing matrices; pinning a
// test ensures a typo in the stdlib doesn't silently remap codes.
func TestMockServiceGRPCStdlibShorthands(t *testing.T) {
	pbPath := writeTestDescriptorSet(t)
	cases := []struct {
		starFn string
		want   grpccodes.Code
	}{
		{"grpc.unavailable()", grpccodes.Unavailable},
		{"grpc.deadline_exceeded()", grpccodes.DeadlineExceeded},
		{"grpc.permission_denied()", grpccodes.PermissionDenied},
		{"grpc.unauthenticated()", grpccodes.Unauthenticated},
		{"grpc.not_found()", grpccodes.NotFound},
		{"grpc.resource_exhausted()", grpccodes.ResourceExhausted},
		{"grpc.internal()", grpccodes.Internal},
	}
	for _, c := range cases {
		t.Run(c.want.String(), func(t *testing.T) {
			port := freePort(t)
			rt := New(testLogger())
			src := fmt.Sprintf(`
load("@faultbox/mocks/grpc.star", "grpc")

geo = grpc.server(
    name        = "geo",
    interface   = interface("main", "grpc", %d),
    descriptors = %q,
    services    = {"/test.geo.GeoService/GetCity": %s},
)
`, port, pbPath, c.starFn)
			if err := rt.LoadString("grpc_shorthand.star", src); err != nil {
				t.Fatalf("LoadString: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := rt.startServices(ctx); err != nil {
				t.Fatalf("startServices: %v", err)
			}
			defer rt.stopServices()

			conn, _ := grpcdial.NewClient(fmt.Sprintf("127.0.0.1:%d", port),
				grpcdial.WithTransportCredentials(insecurecreds.NewCredentials()))
			defer conn.Close()

			var resp anyRecv
			err := conn.Invoke(context.Background(), "/test.geo.GeoService/GetCity",
				&grpcEmptyMsg{}, &resp)
			st, ok := grpcstatus.FromError(err)
			if !ok {
				t.Fatalf("expected grpc status error, got %T: %v", err, err)
			}
			if st.Code() != c.want {
				t.Errorf("%s → code %s, want %s", c.starFn, st.Code(), c.want)
			}
		})
	}
}

// writeTestDescriptorSet materializes a synthetic FileDescriptorSet
// (test.geo.City / GeoService.GetCity) to a temp .pb file and returns
// the path. Same shape as buildCityDescriptorSet() in the protocol
// package; duplicated here to keep the star package test-only dependency
// surface small.
func writeTestDescriptorSet(t *testing.T) string {
	t.Helper()
	syntax := "proto3"
	pkg := "test.geo"
	mkField := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Type: &typ, Label: &label,
		}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("test/geo.proto"), Package: &pkg, Syntax: &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("City"), Field: []*descriptorpb.FieldDescriptorProto{
				mkField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				mkField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				mkField("country", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				mkField("currency", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			}},
			{Name: proto.String("GetCityRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				mkField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("GeoService"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("GetCity"),
				InputType:  proto.String(".test.geo.GetCityRequest"),
				OutputType: proto.String(".test.geo.City"),
			}},
		}},
	}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	raw, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshal set: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.pb")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write pb: %v", err)
	}
	return path
}

// grpcEmptyMsg is a zero-byte request for tests that don't care about input.
type grpcEmptyMsg struct{}

func (*grpcEmptyMsg) Reset()                   {}
func (*grpcEmptyMsg) String() string           { return "empty" }
func (*grpcEmptyMsg) ProtoMessage()            {}
func (*grpcEmptyMsg) Marshal() ([]byte, error) { return nil, nil }
func (*grpcEmptyMsg) Unmarshal([]byte) error   { return nil }

// grpcTypedRecv decodes response bytes into an injected dynamicpb.Message,
// simulating what a compiled *.pb.go client does internally.
type grpcTypedRecv struct {
	msg proto.Message
}

func (r *grpcTypedRecv) Reset()                   {}
func (r *grpcTypedRecv) String() string           { return "grpcTypedRecv" }
func (r *grpcTypedRecv) ProtoMessage()            {}
func (r *grpcTypedRecv) Marshal() ([]byte, error) { return nil, nil }
func (r *grpcTypedRecv) Unmarshal(b []byte) error { return proto.Unmarshal(b, r.msg) }

// anyRecv accepts any response bytes without decoding; used when we expect
// an error response (no body to decode).
type anyRecv struct{}

func (*anyRecv) Reset()                   {}
func (*anyRecv) String() string           { return "anyRecv" }
func (*anyRecv) ProtoMessage()            {}
func (*anyRecv) Marshal() ([]byte, error) { return nil, nil }
func (*anyRecv) Unmarshal([]byte) error   { return nil }

// TestMockServiceMongoStdlib verifies @faultbox/mocks/mongodb.star stands up
// a MongoDB mock and the real mongo driver completes handshake + find.
func TestMockServiceMongoStdlib(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/mongodb.star", "mongo")

users_db = mongo.server(
    name      = "users-stub",
    interface = interface("main", "mongodb", %d),
    collections = {
        "users": [
            {"_id": "1", "name": "alice"},
            {"_id": "2", "name": "bob"},
        ],
    },
)
`, port)
	if err := rt.LoadString("mongo_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	clientOpts := mongoopts.Client().
		ApplyURI("mongodb://" + addr).
		SetServerSelectionTimeout(3 * time.Second).
		SetConnectTimeout(3 * time.Second)
	client, err := mongodriver.Connect(clientOpts)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(context.Background())

	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	cur, err := client.Database("mock").Collection("users").Find(cctx, bsonpkg.M{})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	var results []bsonpkg.M
	if err := cur.All(cctx, &results); err != nil {
		t.Fatalf("cursor.All: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
}

// TestMockServiceGRPC verifies a mock_service declared with protocol grpc
// stands up a grpc-go server and routes unary calls via routes={} +
// grpc_response()/grpc_error(). Exercises the full runtime path.
func TestMockServiceGRPC(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
flags = mock_service("flags",
    interface("main", "grpc", %d),
    routes = {
        "/flags.v1.Flags/Get":  grpc_response(body = {"enabled": True, "variant": "B"}),
        "/flags.v1.Flags/Fail": grpc_error(code = "UNAVAILABLE", message = "backend down"),
    },
)
`, port)
	if err := rt.LoadString("grpc_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := grpcdial.NewClient(addr, grpcdial.WithTransportCredentials(insecurecreds.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var got grpcstructpb.Struct
	if err := conn.Invoke(context.Background(), "/flags.v1.Flags/Get", &emptyGRPC{}, &got); err != nil {
		t.Fatalf("Invoke Get: %v", err)
	}
	if v := got.Fields["variant"].GetStringValue(); v != "B" {
		t.Errorf("variant = %q, want B", v)
	}

	err = conn.Invoke(context.Background(), "/flags.v1.Flags/Fail", &emptyGRPC{}, &got)
	st, ok := grpcstatus.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != grpccodes.Unavailable {
		t.Errorf("code = %s, want Unavailable", st.Code())
	}
}

// emptyGRPC is a zero-byte proto used as the request message for Invoke
// in TestMockServiceGRPC. Local to this file — the grpc mock doesn't
// inspect request bodies in this test.
type emptyGRPC struct{}

func (*emptyGRPC) Reset()                   {}
func (*emptyGRPC) String() string           { return "empty" }
func (*emptyGRPC) ProtoMessage()            {}
func (*emptyGRPC) Marshal() ([]byte, error) { return nil, nil }
func (*emptyGRPC) Unmarshal([]byte) error   { return nil }

// TestMockServiceHTTPWithTLS verifies that tls=True on an HTTP mock results
// in a real TLS listener, and that a client configured to trust the
// runtime's mock CA can reach it over HTTPS.
func TestMockServiceHTTPWithTLS(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
auth = mock_service("auth",
    interface("http", "http", %d),
    tls = True,
    routes = {
        "GET /.well-known/jwks": json_response(status = 200, body = {"keys": [{"kid": "test-1"}]}),
    },
)
`, port)
	if err := rt.LoadString("tls_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	caPath := rt.MockCAPath()
	if caPath == "" {
		t.Fatal("MockCAPath() is empty after starting a tls=True mock")
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("AppendCertsFromPEM: parse failed")
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/.well-known/jwks", port))
	if err != nil {
		t.Fatalf("GET https: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"kid":"test-1"`) {
		t.Fatalf("body = %q", string(body))
	}

	// A client that doesn't trust the CA should fail handshake.
	strict := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
		Timeout:   3 * time.Second,
	}
	_, err = strict.Get(fmt.Sprintf("https://127.0.0.1:%d/.well-known/jwks", port))
	if err == nil {
		t.Fatal("expected strict client to fail TLS verification; got nil")
	}
}

// TestMockServiceGRPCWithTLS verifies that tls=True on a gRPC mock
// terminates TLS before the gRPC handshake and a CA-trusting client
// completes the Invoke round-trip.
func TestMockServiceGRPCWithTLS(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
flags = mock_service("flags",
    interface("main", "grpc", %d),
    tls = True,
    routes = {
        "/flags.v1.Flags/Get": grpc_response(body = {"enabled": True}),
    },
)
`, port)
	if err := rt.LoadString("grpc_tls.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	caPEM, err := os.ReadFile(rt.MockCAPath())
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := grpcdial.NewClient(addr, grpcdial.WithTransportCredentials(
		grpccreds.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost"}),
	))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var got grpcstructpb.Struct
	if err := conn.Invoke(ctx, "/flags.v1.Flags/Get", &emptyGRPC{}, &got); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !got.Fields["enabled"].GetBoolValue() {
		t.Errorf("enabled field missing/false: %+v", got.Fields)
	}
}

// newTestH2CClient returns an HTTP client that speaks h2c. Local to this
// file to avoid reaching into the protocol package's unexported helpers.
func newTestH2CClient() *http.Client {
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

// TestMockServiceOpenAPIStdlib verifies @faultbox/mocks/http.star's
// http.server() constructor loads an OpenAPI 3.0 document, auto-generates
// routes from paths × operations, and serves the declared examples —
// the end-to-end happy path for RFC-021 Phase 1.
func TestMockServiceOpenAPIStdlib(t *testing.T) {
	// Minimal Petstore-style spec. Kept inline so the test is
	// hermetic and doesn't depend on the demo fixture layout.
	specBody := `openapi: 3.0.3
info:
  title: Petstore
  version: "1.0"
paths:
  /pets:
    get:
      responses:
        "200":
          description: list
          content:
            application/json:
              example:
                - id: 1
                  name: fluffy
  /pets/{id}:
    get:
      parameters:
        - name: id
          in: path
          required: true
          schema: {type: integer}
      responses:
        "200":
          description: single
          content:
            application/json:
              example:
                id: 1
                name: fluffy
  /healthz:
    get:
      responses:
        "204":
          description: ok
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "petstore.yaml")
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/http.star", "http")

petstore = http.server(
    name      = "petstore",
    interface = interface("main", "http", %d),
    openapi   = %q,
)
`, port, specPath)
	if err := rt.LoadString("openapi_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}

	// GET /pets → 200, first example (array).
	resp, err := client.Get(base + "/pets")
	if err != nil {
		t.Fatalf("GET /pets: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("GET /pets status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "fluffy") {
		t.Errorf("GET /pets body = %q, want fluffy", body)
	}

	// GET /pets/42 → 200 (path param becomes glob).
	resp, err = client.Get(base + "/pets/42")
	if err != nil {
		t.Fatalf("GET /pets/42: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("GET /pets/42 status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /healthz → 204, empty body (status-only response).
	resp, err = client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("GET /healthz status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// POST /pets → 404 (method not declared for this path).
	resp, err = client.Post(base+"/pets", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /pets: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("POST /pets status = %d, want 404 (method not in spec)", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMockServiceOpenAPIOverrides verifies that overrides={} replaces
// OpenAPI-generated routes by pattern. Users who copy the OpenAPI path
// (with `{id}` placeholders) directly into overrides should see the
// override applied to the auto-generated glob pattern (`*`). RFC-021.
func TestMockServiceOpenAPIOverrides(t *testing.T) {
	specBody := `openapi: 3.0.3
info: {title: Petstore, version: "1.0"}
paths:
  /pets/{id}:
    get:
      parameters:
        - {name: id, in: path, required: true, schema: {type: integer}}
      responses:
        "200":
          description: single
          content:
            application/json:
              example: {id: 1, name: "default"}
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/http.star", "http")

petstore = http.server(
    name      = "petstore",
    interface = interface("main", "http", %d),
    openapi   = %q,
    overrides = {
        # OpenAPI-style path — normalised to /pets/* internally.
        "GET /pets/{id}": json_response(status = 503, body = {"overridden": True}),
    },
)
`, port, specPath)
	if err := rt.LoadString("overrides.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/pets/42", port))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503 (override)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "overridden") {
		t.Errorf("body = %q, want overridden marker", body)
	}
}

// TestMockServiceOpenAPIValidateStrict verifies that validate="strict"
// rejects requests that don't match the operation's requestBody schema
// with HTTP 400, while valid requests still receive the generated response.
func TestMockServiceOpenAPIValidateStrict(t *testing.T) {
	specBody := `openapi: 3.0.3
info: {title: t, version: "1.0"}
paths:
  /pets:
    post:
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: {type: string, minLength: 1}
      responses:
        "201":
          description: created
          content:
            application/json:
              example: {id: 42, name: "new-pet"}
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/http.star", "http")

petstore = http.server(
    name      = "petstore",
    interface = interface("main", "http", %d),
    openapi   = %q,
    validate  = "strict",
)
`, port, specPath)
	if err := rt.LoadString("validate.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	client := &http.Client{Timeout: 2 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Valid: should get 201.
	resp, err := client.Post(base+"/pets", "application/json", strings.NewReader(`{"name":"fluffy"}`))
	if err != nil {
		t.Fatalf("valid POST: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("valid POST status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing required field → 400.
	resp, err = client.Post(base+"/pets", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("invalid POST: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("invalid POST status = %d, want 400 (strict validation)", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMockServiceOpenAPISynthesize verifies that examples="synthesize"
// serves schema-generated placeholder values when no example is declared,
// instead of hard-erroring at route-build time. RFC-021 Phase 3.
func TestMockServiceOpenAPISynthesize(t *testing.T) {
	specBody := `openapi: 3.0.3
info: {title: t, version: "1.0"}
paths:
  /unknown:
    get:
      responses:
        "200":
          description: no example
          content:
            application/json:
              schema:
                type: object
                properties:
                  id:   {type: integer}
                  name: {type: string}
`
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/http.star", "http")

svc = http.server(
    name      = "svc",
    interface = interface("main", "http", %d),
    openapi   = %q,
    examples  = "synthesize",
)
`, port, specPath)
	if err := rt.LoadString("synth.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/unknown", port))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"id":0`) || !strings.Contains(string(body), `"name":""`) {
		t.Errorf("synthesized body = %q, want minimal id=0 name=\"\"", body)
	}
}

// TestMockServiceOpenAPIMalformedFailsAtLoad verifies that a broken
// OpenAPI document surfaces at LoadString time — not at startServices
// time and not at request time. Fail-fast on malformed specs is OQ6's
// resolution for RFC-021.
func TestMockServiceOpenAPIMalformedFailsAtLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("this: is: not: openapi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rt := New(testLogger())
	src := fmt.Sprintf(`
mock_service("x", interface("main", "http", 18091), openapi = %q)
`, path)
	err := rt.LoadString("malformed_openapi.star", src)
	if err == nil {
		t.Fatal("expected LoadString to error on malformed openapi, got nil")
	}
	if !strings.Contains(err.Error(), "openapi") {
		t.Errorf("error should mention openapi, got: %v", err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
