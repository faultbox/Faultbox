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
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// The subprocess is a real strict TLS consumer of the SUT environment. It
// cannot access runtime MockCAPath(), so success requires the declared
// mock.ca_path and rewritten service URL to be independently usable.
func TestAcceptanceTLSClientProcess(t *testing.T) {
	if os.Getenv("FAULTBOX_ACCEPTANCE_TLS_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	pem, err := os.ReadFile(os.Getenv("SSL_CERT_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("declared CA file is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get(os.Getenv("CONFIG_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(body) != "trusted-by-declared-ca" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
		t.Fatal("TLS identity was not verified")
	}
}

func TestAcceptanceMockCAPathTrustedBySUTProcess(t *testing.T) {
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	port := freePort(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
config = mock_service("config", interface("main", "http", %d), tls=True,
    routes={"GET /": text_response(200, "trusted-by-declared-ca")})
sut = service("sut", %q, interface("main", "http", %d), depends_on=[config], env={
    "SSL_CERT_FILE": config.ca_path,
    "CONFIG_URL": "https://localhost:%d/",
    "FAULTBOX_ACCEPTANCE_TLS_HELPER": "1",
})
`, port, executable, freePort(t), port)
	if err := rt.LoadString("declared_ca.star", src); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.startMockService(ctx, "config", rt.services["config"]); err != nil {
		t.Fatal(err)
	}
	if err := rt.preStartProxies(ctx, "config", rt.services["config"]); err != nil {
		t.Fatal(err)
	}
	env := rt.buildEnv(rt.services["sut"])
	expectedURL := "https://" + rt.proxyMgr.GetProxyAddr("config", "main") + "/"
	if actual := envToMap(env)["CONFIG_URL"]; actual != expectedURL {
		t.Fatalf("SUT URL=%q want proxy %q", actual, expectedURL)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestAcceptanceTLSClientProcess$", "-test.count=1")
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("strict TLS SUT: %v\n%s", err, output)
	}
}

// Exercise remote service startup, the public fault scope and cleanup, and
// independent SUT connections. The first call remains in flight when the
// scope returns; a fresh connection must succeed before its delay expires.
func TestAcceptanceRemoteGRPCFaultClearWithInflightRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	src := fmt.Sprintf(`
config = service("config", interface("main", "grpc", %d), remote="127.0.0.1",
    healthcheck=tcp(%q))
def with_delay(run):
    fault(config.main, delay(method="*", delay="1s"), run=run)
def test_remote():
    pass
`, ln.Addr().(*net.TCPAddr).Port, ln.Addr().String())
	if err := rt.LoadString("remote_inflight.star", src); err != nil {
		t.Fatal(err)
	}
	rt.globals["test_remote"] = starlark.NewBuiltin("test_remote", func(thread *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addr := rt.proxyMgr.GetProxyAddr("config", "main")
		if addr == "" {
			return nil, fmt.Errorf("remote proxy was not started")
		}
		call := func(ctx context.Context) error {
			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return err
			}
			defer conn.Close()
			result, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			if err != nil {
				return err
			}
			if result.Status != healthpb.HealthCheckResponse_SERVING {
				return fmt.Errorf("unexpected health status %v", result.Status)
			}
			return nil
		}
		hit := make(chan struct{}, 1)
		id := rt.events.Subscribe(nil, func(event Event) error {
			if event.Type == "proxy" && event.Service == "config" && event.Fields["phase"] == "started" {
				select {
				case hit <- struct{}{}:
				default:
				}
			}
			return nil
		})
		defer rt.events.Unsubscribe(id)
		done := make(chan error, 1)
		start := time.Now()
		launch := starlark.NewBuiltin("launch", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			go func() { done <- call(ctx) }()
			select {
			case <-hit:
				return starlark.None, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("missing delay hit: %w", ctx.Err())
			}
		})
		if _, err := starlark.Call(thread, rt.globals["with_delay"], starlark.Tuple{launch}, nil); err != nil {
			return nil, err
		}
		select {
		case err := <-done:
			return nil, fmt.Errorf("inflight call completed before scope removal: %v", err)
		default:
		}
		quick, cancelQuick := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancelQuick()
		if err := call(quick); err != nil {
			return nil, fmt.Errorf("fresh connection after rule clear: %w", err)
		}
		select {
		case err := <-done:
			if err != nil {
				return nil, fmt.Errorf("inflight call: %w", err)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if time.Since(start) < time.Second {
			return nil, fmt.Errorf("scope removal shortened the original delay")
		}
		return starlark.None, nil
	})
	result := runMockTest(t, rt, "test_remote")
	if result.Result != "pass" || result.FaultBypassed {
		t.Fatalf("result=%s bypass=%v reason=%s", result.Result, result.FaultBypassed, result.Reason)
	}
	if diagnostics := protocolFaultDiagnostics(result.Events); len(diagnostics) != 0 {
		t.Fatalf("false fault diagnostics: %+v", diagnostics)
	}
	var started, removed, completed int64
	var rpcID string
	for _, event := range result.Events {
		if event.Service != "config" {
			continue
		}
		switch {
		case event.Type == "proxy" && event.Fields["phase"] == "started":
			started, rpcID = event.Seq, event.Fields["rpc_id"]
		case event.Type == "proxy_fault_removed":
			removed = event.Seq
		case event.Type == "proxy_delay_completed" && event.Fields["rpc_id"] == rpcID:
			completed = event.Seq
		}
	}
	if started == 0 || removed <= started || completed <= removed || strings.TrimSpace(rpcID) == "" {
		t.Fatalf("event order started=%d removed=%d completed=%d rpcID=%q", started, removed, completed, rpcID)
	}
}
