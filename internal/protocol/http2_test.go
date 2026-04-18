package protocol

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestHTTP2ProtocolRegistered(t *testing.T) {
	p, ok := Get("http2")
	if !ok {
		t.Fatal("http2 protocol not registered")
	}
	want := []string{"get", "post", "put", "delete", "patch"}
	if !reflect.DeepEqual(p.Methods(), want) {
		t.Errorf("Methods() = %v, want %v", p.Methods(), want)
	}
}

// TestHTTP2_GetAgainstH2CServer verifies the client speaks HTTP/2 over
// cleartext and that the response comes back decoded. This is the hot path
// for service-mesh traffic.
func TestHTTP2_GetAgainstH2CServer(t *testing.T) {
	ln, server := startTestH2CServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Proto != "HTTP/2.0" {
			t.Errorf("server saw proto %q, want HTTP/2.0", r.Proto)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"method":%q,"path":%q}`, r.Method, r.URL.Path)
	})
	defer server.Close()

	p := &http2Protocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, ln.Addr().String(), "get", map[string]any{"path": "/hello"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !res.Success {
		t.Fatalf("step failed: %s", res.Error)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(res.Body, `"path":"/hello"`) {
		t.Errorf("body = %q, missing path", res.Body)
	}
	if res.Fields["proto"] != "HTTP/2.0" {
		t.Errorf("proto field = %q, want HTTP/2.0", res.Fields["proto"])
	}
}

func TestHTTP2_HealthcheckSucceeds(t *testing.T) {
	ln, server := startTestH2CServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	defer server.Close()

	p := &http2Protocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := p.Healthcheck(ctx, ln.Addr().String(), 3*time.Second); err != nil {
		t.Errorf("Healthcheck: %v", err)
	}
}

func TestHTTP2_UnsupportedMethod(t *testing.T) {
	p := &http2Protocol{}
	_, err := p.ExecuteStep(context.Background(), "127.0.0.1:1", "propfind", nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected unsupported method error, got %v", err)
	}
}

// startTestH2CServer runs a minimal h2c server on a random port so client
// tests don't need TLS certs.
func startTestH2CServer(t *testing.T, handler http.HandlerFunc) (net.Listener, *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: h2c.NewHandler(handler, &http2.Server{}),
	}
	go srv.Serve(ln)
	return ln, srv
}
