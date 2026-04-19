package protocol

import (
	"context"
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

	"github.com/segmentio/kafka-go"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoopts "go.mongodb.org/mongo-driver/v2/mongo/options"
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
