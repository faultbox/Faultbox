package protocol

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/segmentio/kafka-go"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoopts "go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestHTTPMockStaticRoutes verifies that the HTTP MockHandler matches
// METHOD PATTERN routes, returns the configured status/body/headers, and
// falls back to 404 when no route matches.
func TestHTTPMockStaticRoutes(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "GET /.well-known/openid-configuration/jwks",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"keys":[{"kty":"OKP"}]}`),
					ContentType: "application/json",
				},
			},
			{
				Pattern: "GET /health",
				Response: &MockResponse{Status: 204},
			},
			{
				Pattern: "POST /api/v1/**",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"ok":true}`),
					ContentType: "application/json",
					Headers:     map[string]string{"X-Mock": "1"},
				},
			},
		},
	}

	emitted := &recordedEmits{}
	serveErr := make(chan error, 1)
	go func() { serveErr <- p.ServeMock(ctx, addr, spec, emitted.emit) }()

	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}

	// Route 1: JWKS endpoint.
	resp, err := client.Get(fmt.Sprintf("http://%s/.well-known/openid-configuration/jwks", addr))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("jwks status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"kty":"OKP"`) {
		t.Fatalf("jwks body = %q", string(body))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("jwks content-type = %q", resp.Header.Get("Content-Type"))
	}

	// Route 2: status-only.
	resp, err = client.Get(fmt.Sprintf("http://%s/health", addr))
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("health status = %d, want 204", resp.StatusCode)
	}

	// Route 3: glob ** match + custom header.
	resp, err = client.Post(fmt.Sprintf("http://%s/api/v1/users/42", addr), "application/json", strings.NewReader(`{"id":42}`))
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Mock") != "1" {
		t.Fatalf("api X-Mock = %q", resp.Header.Get("X-Mock"))
	}

	// Unmatched → 404.
	resp, err = client.Get(fmt.Sprintf("http://%s/does-not-exist", addr))
	if err != nil {
		t.Fatalf("GET unmatched: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unmatched status = %d, want 404", resp.StatusCode)
	}

	if got := emitted.count(); got != 4 {
		t.Fatalf("emitter called %d times, want 4", got)
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTPMockDynamicHandler verifies that a dynamic handler receives
// request context and that its returned MockResponse is written back.
func TestHTTPMockDynamicHandler(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	var observed MockRequest
	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "POST /token",
				Dynamic: func(req MockRequest) (*MockResponse, error) {
					observed = req
					body, _ := json.Marshal(map[string]string{
						"user": req.Query["user"],
						"body": string(req.Body),
					})
					return &MockResponse{
						Status:      200,
						Body:        body,
						ContentType: "application/json",
					}, nil
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://%s/token?user=alice", addr),
		"application/json",
		strings.NewReader(`{"hello":"world"}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if observed.Method != "POST" {
		t.Fatalf("observed.Method = %q", observed.Method)
	}
	if observed.Query["user"] != "alice" {
		t.Fatalf("observed.Query[user] = %q", observed.Query["user"])
	}
	if string(observed.Body) != `{"hello":"world"}` {
		t.Fatalf("observed.Body = %q", string(observed.Body))
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"user":"alice"`) {
		t.Fatalf("response body = %q", string(body))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTPMockDefaultResponse verifies that unmatched requests fall through
// to the configured default when set.
func TestHTTPMockDefaultResponse(t *testing.T) {
	p := &httpProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Default: &MockResponse{
			Status:      501,
			Body:        []byte(`unsupported`),
			ContentType: "text/plain",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/anything", addr))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// TestMatchGlob covers the path glob patterns directly.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		target  string
		want    bool
	}{
		{"/health", "/health", true},
		{"/health", "/health/v2", false},
		{"/api/*", "/api/users", true},
		{"/api/*", "/api/users/42", false},
		{"/api/**", "/api/users/42/posts", true},
		{"/api/**/posts", "/api/users/42/posts", true},
		{"/api/**/posts", "/api/42/users/posts", true},
		{"/api/**/posts", "/api/42/users", false},
		{"/.well-known/openid-configuration/jwks", "/.well-known/openid-configuration/jwks", true},
	}
	for _, tc := range cases {
		got := matchGlob(tc.pattern, tc.target)
		if got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
		}
	}
}

// TestHTTP2MockStaticRoutes verifies the HTTP/2 MockHandler over h2c —
// same route table as HTTP, served over cleartext HTTP/2. Clients connect
// with an h2c-capable transport to confirm the response protocol is HTTP/2.
func TestHTTP2MockStaticRoutes(t *testing.T) {
	p := &http2Protocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern: "GET /healthz",
				Response: &MockResponse{Status: 200, Body: []byte(`ok`), ContentType: "text/plain"},
			},
			{
				Pattern: "POST /api/v1/**",
				Response: &MockResponse{
					Status:      200,
					Body:        []byte(`{"ok":true}`),
					ContentType: "application/json",
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := newH2CClient(2 * time.Second)

	// Route 1 — verify protocol is HTTP/2.0.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("healthz proto = %q, want HTTP/2.0", resp.Proto)
	}
	if string(body) != "ok" {
		t.Fatalf("healthz body = %q", string(body))
	}

	// Route 2 — glob ** match.
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/api/v1/orders/42", strings.NewReader(`{"x":1}`))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST api: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("api proto = %q, want HTTP/2.0", resp.Proto)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("api body = %q", string(body))
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestHTTP2MockDynamicHandler verifies dynamic handlers work over h2c too.
func TestHTTP2MockDynamicHandler(t *testing.T) {
	p := &http2Protocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{{
			Pattern: "POST /echo",
			Dynamic: func(req MockRequest) (*MockResponse, error) {
				payload, _ := json.Marshal(map[string]string{
					"method": req.Method,
					"path":   req.Path,
					"body":   string(req.Body),
				})
				return &MockResponse{Status: 200, Body: payload, ContentType: "application/json"}, nil
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	client := newH2CClient(2 * time.Second)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/echo", strings.NewReader(`{"ping":"pong"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST echo: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("echo proto = %q, want HTTP/2.0", resp.Proto)
	}
	if !strings.Contains(string(body), `"body":"{\"ping\":\"pong\"}"`) {
		t.Fatalf("echo body = %q", string(body))
	}
}

// TestTCPMockLineFramed verifies byte-prefix matching on line-framed input.
func TestTCPMockLineFramed(t *testing.T) {
	p := &tcpProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{
			{Pattern: "PING\n", Response: &MockResponse{Body: []byte("PONG\n")}},
			{Pattern: "VERSION", Response: &MockResponse{Body: []byte("2.0.0\n")}},
		},
		Default: &MockResponse{Body: []byte("ERR unknown\n")},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	cases := []struct {
		send, want string
	}{
		{"PING\n", "PONG\n"},
		{"VERSION\n", "2.0.0\n"},
		{"BOGUS\n", "ERR unknown\n"},
	}
	for _, tc := range cases {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial %q: %v", tc.send, err)
		}
		_, _ = conn.Write([]byte(tc.send))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Close()
		if string(buf[:n]) != tc.want {
			t.Errorf("send %q: got %q, want %q", tc.send, buf[:n], tc.want)
		}
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// TestTCPMockDynamicHandler exercises the dynamic path for TCP.
func TestTCPMockDynamicHandler(t *testing.T) {
	p := &tcpProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{{
			Pattern: "ECHO ",
			Dynamic: func(req MockRequest) (*MockResponse, error) {
				// Reflect the body back, prefixed with ACK.
				return &MockResponse{Body: append([]byte("ACK "), req.Body...)}, nil
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mock not listening: %v", err)
	}

	conn, _ := net.DialTimeout("tcp", addr, time.Second)
	_, _ = conn.Write([]byte("ECHO hello\n"))
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	conn.Close()
	if string(buf[:n]) != "ACK ECHO hello\n" {
		t.Fatalf("echo response = %q", buf[:n])
	}
}

// TestUDPMockSwallowAndRecord verifies the StatsD pattern: no routes, no
// response, every datagram emitted as an event.
func TestUDPMockSwallowAndRecord(t *testing.T) {
	p := &udpProtocol{}
	addr := freeLoopbackAddr(t)

	emitted := &recordedEmits{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, MockSpec{}, emitted.emit)

	// Brief wait for listen.
	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, _ = conn.Write([]byte(fmt.Sprintf("metric:%d|c", i)))
	}
	conn.Close()

	// Let the goroutine catch up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if emitted.count() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := emitted.count(); got < 3 {
		t.Fatalf("emitter called %d times, want >=3", got)
	}
}

// TestUDPMockPrefixResponse verifies prefix matching + response write-back.
func TestUDPMockPrefixResponse(t *testing.T) {
	p := &udpProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Routes: []MockRoute{
			{Pattern: "PING", Response: &MockResponse{Body: []byte("PONG")}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	time.Sleep(50 * time.Millisecond)

	conn, _ := net.Dial("udp", addr)
	defer conn.Close()
	_, _ = conn.Write([]byte("PING\n"))
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "PONG" {
		t.Fatalf("response = %q, want PONG", buf[:n])
	}
}

// TestKafkaMockBasic verifies the Kafka mock stands up an in-process broker
// via kfake and a real kafka-go client can connect + list the seeded
// topics. End-to-end produce/consume is covered by a spec-level test.
func TestKafkaMockBasic(t *testing.T) {
	p := &kafkaProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Config: map[string]any{
			"topics": map[string]any{
				"orders":   []any{},
				"payments": []any{},
			},
			"partitions": 1,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()

	if err := waitKafkaMockReady(addr, 3*time.Second); err != nil {
		t.Fatalf("kafka mock not ready: %v", err)
	}

	conn, err := kafka.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("kafka dial: %v", err)
	}
	partitions, err := conn.ReadPartitions()
	conn.Close()
	if err != nil {
		t.Fatalf("ReadPartitions: %v", err)
	}

	seen := make(map[string]bool)
	for _, p := range partitions {
		seen[p.Topic] = true
	}
	for _, want := range []string{"orders", "payments"} {
		if !seen[want] {
			t.Errorf("topic %q not found in metadata; got %+v", want, seen)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

// waitKafkaMockReady polls the broker by opening a Kafka Metadata request
// until it succeeds. kfake needs a moment after NewCluster returns before
// it's fully answering on the port.
func waitKafkaMockReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := kafka.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_, err = conn.ReadPartitions()
			conn.Close()
			if err == nil {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("kafka mock not answering at %s", addr)
}

// TestRedisMockBasic verifies the Redis mock stands up via miniredis and a
// real redis client (raw RESP over TCP for minimal deps) can GET + SET +
// INCR + read seeded state.
func TestRedisMockBasic(t *testing.T) {
	p := &redisProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Config: map[string]any{
			"state": map[string]any{
				"config:timeout": "5000",
				"flag:new_ui":    "true",
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("redis mock not listening: %v", err)
	}

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	// Seeded GET
	if got := redisRoundtrip(t, conn, "GET", "config:timeout"); got != "5000" {
		t.Errorf("GET config:timeout = %q, want 5000", got)
	}
	// SET + GET
	if got := redisRoundtrip(t, conn, "SET", "k", "v"); got != "OK" {
		t.Errorf("SET = %q", got)
	}
	if got := redisRoundtrip(t, conn, "GET", "k"); got != "v" {
		t.Errorf("GET k = %q, want v", got)
	}
	// INCR
	if got := redisRoundtrip(t, conn, "INCR", "counter"); got != "1" {
		t.Errorf("INCR counter = %q, want 1", got)
	}
	if got := redisRoundtrip(t, conn, "INCR", "counter"); got != "2" {
		t.Errorf("INCR counter (2) = %q, want 2", got)
	}
}

// redisRoundtrip sends an inline Redis command and reads one RESP reply.
// Strips type markers (+, :, $) and returns the raw payload. Minimal
// parser — sufficient for the string-cache test surface.
func redisRoundtrip(t *testing.T, conn net.Conn, args ...string) string {
	t.Helper()
	// Build a RESP array of bulk strings.
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	resp := string(buf[:n])
	resp = strings.TrimRight(resp, "\r\n")
	// Trim the RESP type byte and, for bulk strings, the length line.
	switch {
	case strings.HasPrefix(resp, "+"):
		return resp[1:]
	case strings.HasPrefix(resp, ":"):
		return resp[1:]
	case strings.HasPrefix(resp, "$"):
		lines := strings.SplitN(resp, "\r\n", 2)
		if len(lines) == 2 {
			return lines[1]
		}
		return resp
	case strings.HasPrefix(resp, "-"):
		return "ERR:" + resp[1:]
	default:
		return resp
	}
}

// TestMongoMockHandshakeAndFind verifies the MongoDB mock completes the
// driver handshake and returns seeded documents via a raw OP_MSG round-trip.
func TestMongoMockHandshakeAndFind(t *testing.T) {
	p := &mongoProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Config: map[string]any{
			"collections": map[string]any{
				"users": []any{
					map[string]any{"_id": "1", "name": "alice"},
					map[string]any{"_id": "2", "name": "bob"},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeMock(ctx, addr, spec, nil)
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("mongo mock not listening: %v", err)
	}

	// Use the real mongo driver to verify end-to-end wire compatibility.
	clientOpts := mongoopts.Client().
		ApplyURI("mongodb://" + addr).
		SetServerSelectionTimeout(3 * time.Second).
		SetConnectTimeout(3 * time.Second)
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer client.Disconnect(context.Background())

	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()

	// Ping exercises the handshake path.
	if err := client.Ping(cctx, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Find on a seeded collection.
	cur, err := client.Database("mock").Collection("users").Find(cctx, bson.M{})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	var results []bson.M
	if err := cur.All(cctx, &results); err != nil {
		t.Fatalf("cursor.All: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (%+v)", len(results), results)
	}
	names := map[string]bool{}
	for _, r := range results {
		if n, ok := r["name"].(string); ok {
			names[n] = true
		}
	}
	if !names["alice"] || !names["bob"] {
		t.Errorf("expected alice+bob, got %v", names)
	}
}

// TestGRPCMockUnaryCall verifies that a gRPC mock routes /pkg.Svc/Method
// correctly, returns a typeless struct response, and maps grpc_error() to
// a status code the client sees.
func TestGRPCMockUnaryCall(t *testing.T) {
	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)

	okBytes, err := JSONToGRPCStruct([]byte(`{"enabled":true,"variant":"B"}`))
	if err != nil {
		t.Fatalf("encode struct: %v", err)
	}

	spec := MockSpec{
		Routes: []MockRoute{
			{
				Pattern:  "/flags.v1.Flags/Get",
				Response: &MockResponse{Status: 0, Body: okBytes},
			},
			{
				Pattern: "/flags.v1.Flags/Fail",
				Response: &MockResponse{
					Status: 14, // UNAVAILABLE
					Body:   []byte("backend down"),
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Happy path: /flags.v1.Flags/Get — decode Struct.
	var gotStruct structpb.Struct
	err = conn.Invoke(context.Background(), "/flags.v1.Flags/Get", &emptyMsg{}, &gotStruct)
	if err != nil {
		t.Fatalf("Invoke Get: %v", err)
	}
	if got := gotStruct.Fields["variant"].GetStringValue(); got != "B" {
		t.Errorf("variant = %q, want B; got struct=%+v", got, gotStruct.Fields)
	}

	// Error path: /flags.v1.Flags/Fail — expect status UNAVAILABLE.
	err = conn.Invoke(context.Background(), "/flags.v1.Flags/Fail", &emptyMsg{}, &gotStruct)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %T: %v", err, err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %s, want Unavailable", st.Code())
	}
	if st.Message() != "backend down" {
		t.Errorf("error msg = %q", st.Message())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("grpc mock did not shut down in time")
	}
}

// TestGRPCMockTypedResponse verifies the RFC-023 typed path: with a
// FileDescriptorSet provided on MockSpec.Descriptors, route response
// bodies are treated as JSON and encoded at request-time against each
// method's output message descriptor. A compiled-stub-style client
// decodes the wire bytes cleanly into the expected message type —
// which the Struct-based path can't satisfy because the wire bytes
// would carry a google.protobuf.Struct instead.
func TestGRPCMockTypedResponse(t *testing.T) {
	// Synthetic descriptor set matching buildCityDescriptorSet in
	// grpc_typed_encoder_test.go: service test.geo.GeoService with a
	// GetCity method returning test.geo.City{id, name, country, currency}.
	fdsPath := writeFds(t, buildCityDescriptorSet())
	files, err := LoadDescriptorSet(fdsPath)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Descriptors: files,
		Routes: []MockRoute{
			{
				// In typed mode the Body is raw JSON — the handler encodes
				// it against the method's output descriptor at request time.
				Pattern: "/test.geo.GeoService/GetCity",
				Response: &MockResponse{
					Body: []byte(`{"id":42,"name":"Almaty","country":"KZ","currency":"KZT"}`),
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Simulate the customer's compiled-stub client side by decoding the
	// response via dynamicpb against the expected City descriptor — this
	// is the exact decode path a generated *.pb.go client uses under the
	// hood. If we were still emitting Struct, this Unmarshal would
	// produce zero values.
	cityDesc, _ := files.FindDescriptorByName("test.geo.City")
	cityMd := cityDesc.(protoreflect.MessageDescriptor)
	got := dynamicpb.NewMessage(cityMd)

	err = conn.Invoke(context.Background(), "/test.geo.GeoService/GetCity", &emptyMsg{}, &rawRecvMsg{msg: got})
	if err != nil {
		t.Fatalf("Invoke GetCity: %v", err)
	}

	if v := got.Get(cityMd.Fields().ByName("id")).Int(); v != 42 {
		t.Errorf("City.id = %d, want 42", v)
	}
	if v := got.Get(cityMd.Fields().ByName("name")).String(); v != "Almaty" {
		t.Errorf("City.name = %q, want Almaty", v)
	}
	if v := got.Get(cityMd.Fields().ByName("country")).String(); v != "KZ" {
		t.Errorf("City.country = %q, want KZ", v)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("grpc mock did not shut down in time")
	}
}

// TestGRPCMockTypedResponse_RawEscapeHatch verifies grpc.raw_response():
// with ContentType set to GRPCRawBodyContentType, the handler skips the
// typed encoder and ships Body as-is even when Descriptors is non-nil.
// Power-user escape for oneof tricks / extensions / fields the typed
// encoder can't express.
func TestGRPCMockTypedResponse_RawEscapeHatch(t *testing.T) {
	fdsPath := writeFds(t, buildCityDescriptorSet())
	files, _ := LoadDescriptorSet(fdsPath)

	// Pre-encode a valid City message as raw wire bytes, then ship it
	// through the raw path. If the handler ignored ContentType and
	// tried to JSON-parse these wire bytes, it would fail.
	cityDesc, _ := files.FindDescriptorByName("test.geo.City")
	cityMd := cityDesc.(protoreflect.MessageDescriptor)
	rawWire, err := JSONToTypedMessage(files, cityMd, []byte(`{"id":7,"name":"raw"}`))
	if err != nil {
		t.Fatalf("pre-encode: %v", err)
	}

	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)
	spec := MockSpec{
		Descriptors: files,
		Routes: []MockRoute{{
			Pattern: "/test.geo.GeoService/GetCity",
			Response: &MockResponse{
				Body:        rawWire,
				ContentType: GRPCRawBodyContentType,
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()

	got := dynamicpb.NewMessage(cityMd)
	if err := conn.Invoke(context.Background(), "/test.geo.GeoService/GetCity", &emptyMsg{}, &rawRecvMsg{msg: got}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if v := got.Get(cityMd.Fields().ByName("id")).Int(); v != 7 {
		t.Errorf("City.id = %d, want 7 (raw path)", v)
	}

	cancel()
	<-done
}

// TestGRPCMockReflection verifies that when a typed mock has a descriptor
// set, it serves the gRPC reflection v1 service — grpcurl-style tooling
// can list services and get their methods without a .proto file. RFC-023
// resolved question 6 (reflection = YES, ship it).
//
// Exercises the ListServices request (what services does this server
// expose?). A full round-trip through grpcurl is not feasible from a
// Go test, but the reflection protocol itself is standard gRPC — hitting
// it with the generated ServerReflection client proves the same wire
// exchange grpcurl would.
func TestGRPCMockReflection(t *testing.T) {
	fdsPath := writeFds(t, buildCityDescriptorSet())
	files, err := LoadDescriptorSet(fdsPath)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Descriptors: files,
		Routes: []MockRoute{{
			Pattern: "/test.geo.GeoService/GetCity",
			Response: &MockResponse{
				Body: []byte(`{"id":1,"name":"x"}`),
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	refClient := v1reflectionpb.NewServerReflectionClient(conn)
	stream, err := refClient.ServerReflectionInfo(context.Background())
	if err != nil {
		t.Fatalf("ServerReflectionInfo: %v", err)
	}

	// Request: list all services.
	req := &v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_ListServices{},
	}
	if err := stream.Send(req); err != nil {
		t.Fatalf("send ListServices: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ListServices: %v", err)
	}
	listResp := resp.GetListServicesResponse()
	if listResp == nil {
		t.Fatalf("expected ListServicesResponse, got %T", resp.MessageResponse)
	}

	// Expect our customer service in the list. Reflection service itself
	// also appears (grpc.reflection.v1.ServerReflection); we just look
	// for our service explicitly.
	var found bool
	var names []string
	for _, svc := range listResp.Service {
		names = append(names, svc.Name)
		if svc.Name == "test.geo.GeoService" {
			found = true
		}
	}
	if !found {
		t.Errorf("test.geo.GeoService not in reflection list, got: %v", names)
	}

	_ = stream.CloseSend()

	// Also exercise FileContainingSymbol — the query grpcurl uses to
	// materialize a .proto-like view when describing a method. Confirms
	// the DescriptorResolver is actually wired to our per-mock registry.
	stream2, err := refClient.ServerReflectionInfo(context.Background())
	if err != nil {
		t.Fatalf("ServerReflectionInfo (symbol): %v", err)
	}
	if err := stream2.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "test.geo.GeoService",
		},
	}); err != nil {
		t.Fatalf("send FileContainingSymbol: %v", err)
	}
	resp2, err := stream2.Recv()
	if err != nil {
		t.Fatalf("recv FileContainingSymbol: %v", err)
	}
	fdResp := resp2.GetFileDescriptorResponse()
	if fdResp == nil {
		t.Fatalf("expected FileDescriptorResponse, got %T", resp2.MessageResponse)
	}
	if len(fdResp.FileDescriptorProto) == 0 {
		t.Errorf("no file descriptors returned for test.geo.GeoService")
	}

	_ = stream2.CloseSend()
	cancel()
	<-done
}

// TestGRPCMockReflection_DisabledForUntypedMocks verifies the reflection
// service is NOT registered when a mock has no descriptor set — keeps
// the existing behavior for untyped gRPC mocks (no surprise auto-
// registration for specs that don't opt in to RFC-023).
func TestGRPCMockReflection_DisabledForUntypedMocks(t *testing.T) {
	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)

	okBytes, _ := JSONToGRPCStruct([]byte(`{}`))
	spec := MockSpec{
		// No Descriptors field — untyped Struct path.
		Routes: []MockRoute{{
			Pattern:  "/pkg.Svc/Method",
			Response: &MockResponse{Body: okBytes},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()

	refClient := v1reflectionpb.NewServerReflectionClient(conn)
	stream, err := refClient.ServerReflectionInfo(context.Background())
	if err != nil {
		t.Fatalf("open reflection stream: %v", err)
	}
	if err := stream.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_ListServices{},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Expect the request to hit our UnknownServiceHandler (no reflection
	// service is registered) — response is a gRPC Unimplemented status.
	_, err = stream.Recv()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error for reflection on untyped mock, got %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Errorf("reflection on untyped mock: code = %s, want Unimplemented", st.Code())
	}

	_ = stream.CloseSend()
	cancel()
	<-done
}

// TestGRPCMockConnectInterop verifies that a connectrpc.com/connect
// client configured with connect.WithGRPC() successfully invokes a
// typed mock. RFC-023 resolved question 5 — we expected this to work
// because connect-go in gRPC mode speaks the standard gRPC wire
// protocol on unary calls, but the spec-authoring path is different
// enough that we exercise it explicitly.
//
// Two protocols exist in connect-go:
//   - WithGRPC():    standard gRPC wire (what this test covers)
//   - WithGRPCWeb(): browser-friendly HTTP/1.1 framing — not supported
//                    by the stdlib grpc.Server; would need a separate
//                    handler if a customer asks.
//
// The pure Connect protocol (JSON-over-HTTP) is also not supported by
// the stdlib grpc.Server. If a customer hits either case, RFC-023
// Phase 5 spins up the connect-go handler alongside grpc-go's.
func TestGRPCMockConnectInterop(t *testing.T) {
	fdsPath := writeFds(t, buildCityDescriptorSet())
	files, err := LoadDescriptorSet(fdsPath)
	if err != nil {
		t.Fatalf("LoadDescriptorSet: %v", err)
	}

	p := &grpcProtocol{}
	addr := freeLoopbackAddr(t)

	spec := MockSpec{
		Descriptors: files,
		Routes: []MockRoute{{
			Pattern: "/test.geo.GeoService/GetCity",
			Response: &MockResponse{
				Body: []byte(`{"id":99,"name":"connect-was-here","country":"CN","currency":"CNY"}`),
			},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ServeMock(ctx, addr, spec, nil) }()
	if err := waitListening(addr, 3*time.Second); err != nil {
		t.Fatalf("grpc mock not listening: %v", err)
	}

	// connect-go's generic client requires concrete message types that
	// can be zero-constructed (it does `new(Res)`). dynamicpb.Message
	// zero-value has no descriptor → proto.Unmarshal panics. For this
	// wire-level interop check, use emptypb.Empty on both sides:
	// proto3 discards unknown fields on unmarshal, so the test.geo.City
	// wire bytes the server returns decode into Empty without error.
	// That's enough to verify "connect-go in gRPC mode successfully
	// completes an RPC against our mock" — which is OQ5's actual
	// question. Customers use generated stubs in real code, where the
	// response type is concrete and decodes normally.
	h2cClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(_ context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, a)
			},
		},
	}

	client := connect.NewClient[emptypb.Empty, emptypb.Empty](
		h2cClient,
		"http://"+addr+"/test.geo.GeoService/GetCity",
		connect.WithGRPC(),
	)

	req := connect.NewRequest(&emptypb.Empty{})
	resp, err := client.CallUnary(context.Background(), req)
	if err != nil {
		t.Fatalf("CallUnary via connect-go (gRPC mode): %v", err)
	}
	if resp.Msg == nil {
		t.Fatal("connect-go returned nil message")
	}
	// The wire bytes the server emits encode a test.geo.City. proto3
	// unknown-field semantics let us decode those into Empty cleanly —
	// the verification is "no error, got a proto message back."

	cancel()
	<-done
}

// rawRecvMsg satisfies grpc-go's proto codec by exposing Marshal/Unmarshal
// on raw bytes — delegates the actual decode to an injected proto.Message
// so the test can assert field values post-decode.
type rawRecvMsg struct {
	msg proto.Message
}

func (r *rawRecvMsg) Reset()                   {}
func (r *rawRecvMsg) String() string           { return "rawRecvMsg" }
func (r *rawRecvMsg) ProtoMessage()            {}
func (r *rawRecvMsg) Marshal() ([]byte, error) { return nil, nil }
func (r *rawRecvMsg) Unmarshal(b []byte) error { return proto.Unmarshal(b, r.msg) }

// emptyMsg is a zero-byte proto message used to satisfy grpc-go's codec
// when we don't care about the request body. Send as Invoke's req arg.
type emptyMsg struct{}

func (*emptyMsg) Reset()                   {}
func (*emptyMsg) String() string           { return "empty" }
func (*emptyMsg) ProtoMessage()            {}
func (*emptyMsg) Marshal() ([]byte, error) { return nil, nil }
func (*emptyMsg) Unmarshal([]byte) error   { return nil }

// --- test helpers ---

type recordedEmits struct {
	mu    sync.Mutex
	count_ int64
}

func (r *recordedEmits) emit(op string, fields map[string]string) {
	atomic.AddInt64(&r.count_, 1)
}

func (r *recordedEmits) count() int64 {
	return atomic.LoadInt64(&r.count_)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitListening(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("listener on %s not ready after %s", addr, timeout)
}
