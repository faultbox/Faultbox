package star

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/engine"
	"go.starlark.net/starlark"
)

func TestMockGRPCProxyFaultAndRecovery(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, `
def test_proxy():
    assert_eq(api.get_setting(id=42).data["name"], "DENIED")
    def unavailable():
        result=api.get_setting(id=42)
        assert_true("Unavailable" in result.error)
    fault(config.main, error(method="/test.config.ConfigService/GetSetting",status=14),run=unavailable)
    assert_eq(api.get_setting(id=42).data["name"], "DENIED")
`)
	tr := runMockTest(t, rt, "test_proxy")
	if tr.Result != "pass" || tr.FaultBypassed {
		t.Fatalf("%+v", tr)
	}
	if diags := protocolFaultDiagnostics(tr.Events); len(diags) > 0 {
		t.Fatal(diags)
	}
}

func TestMockUnusedProtocolFaultIsVisible(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, `
def test_unused():
    def body():
        assert_true(api.get_setting(id=42).ok)
    fault(config.main, error(method="/never/called",status=14),run=body)
`)
	tr := runMockTest(t, rt, "test_unused")
	if tr.Result != "pass" || !tr.FaultBypassed {
		t.Fatalf("result=%s bypassed=%v reason=%s", tr.Result, tr.FaultBypassed, tr.Reason)
	}
	d := protocolFaultDiagnostics(tr.Events)
	if len(d) != 1 || d[0].Code != "FAULT_NOT_FIRED" {
		t.Fatalf("%+v", d)
	}
}

func TestMockProxyTLSAndExposedCA(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("tls.star", fmt.Sprintf(`
m=mock_service("m",interface("main","http",%d),tls=True,routes={"GET /":text_response(200,"trusted")})
ca=m.ca_path
`, freePort(t))); err != nil {
		t.Fatal(err)
	}
	ca := string(rt.globals["ca"].(starlark.String))
	data, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("CA invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatal(err)
	}
	defer rt.cleanup()
	addr := rt.proxyMgr.GetProxyAddr("m", "main")
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "trusted" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	svc := &ServiceDef{Name: "sut", Env: map[string]string{"MOCK": fmt.Sprintf("localhost:%d", rt.services["m"].Interfaces["main"].Port)}}
	if got := envToMap(rt.buildEnv(svc))["MOCK"]; got != addr {
		t.Fatalf("binary env=%s want %s", got, addr)
	}
	_, port, _ := splitHostPort(addr)
	if got := envToMap(rt.buildContainerEnv(svc))["MOCK"]; got != fmt.Sprintf("host.docker.internal:%d", port) {
		t.Fatalf("container env=%s", got)
	}
}

func TestStaticTypedMockFailsAtLoad(t *testing.T) {
	pb := writeTestDescriptorSet(t)
	rt := New(testLogger())
	err := rt.LoadString("invalid.star", fmt.Sprintf(`
m=mock_service("m",interface("main","grpc",12345),descriptors=%q,routes={"/test.config.ConfigService/GetSetting":grpc_typed_response(body={"bad_field":1})})
`, pb))
	if err == nil || !strings.Contains(err.Error(), "MOCK_ENCODE_ERROR") {
		t.Fatalf("%v", err)
	}
}

func TestTeardownStopsDependentsBeforeMocks(t *testing.T) {
	rt := New(testLogger())
	rt.order = []string{"upstream", "sut"}
	var order []string
	for _, name := range rt.order {
		done := make(chan *engine.Result)
		rt.sessions[name] = &runningSession{cancel: func() { order = append(order, name); close(done) }, done: done}
	}
	rt.stopServices()
	if strings.Join(order, ",") != "sut,upstream" {
		t.Fatalf("stop order %v", order)
	}
}
