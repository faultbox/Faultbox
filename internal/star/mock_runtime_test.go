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
	grpcstructpb "google.golang.org/protobuf/types/known/structpb"
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

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
