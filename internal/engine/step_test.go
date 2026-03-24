package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/config"
)

func TestRunHTTPStep_GET(t *testing.T) {
	// Start a test HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/data/key1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(200)
			fmt.Fprintln(w, "stored: key1=value1")
			return
		}
		w.WriteHeader(200)
		fmt.Fprintln(w, "value1")
	})
	ln, port := listenOnFreePort(t)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	topo := &config.TopologyConfig{
		Services: map[string]config.ServiceConfig{
			"api": {
				Interfaces: map[string]config.InterfaceConfig{
					"public": {Protocol: "http", Port: port},
				},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	steps := []config.StepConfig{
		{Action: "api.get", Args: map[string]any{
			"path":   "/health",
			"expect": map[string]any{"status": 200, "body_contains": "ok"},
		}},
		{Action: "api.post", Args: map[string]any{
			"path":   "/data/key1",
			"body":   "value1",
			"expect": map[string]any{"status": 200},
		}},
		{Action: "api.get", Args: map[string]any{
			"path":   "/data/key1",
			"expect": map[string]any{"status": 200, "body_equals": "value1"},
		}},
	}

	results, err := RunSteps(context.Background(), topo, steps, log)
	if err != nil {
		t.Fatalf("RunSteps: %v", err)
	}

	for i, r := range results {
		if !r.Success {
			t.Errorf("step[%d] %s failed: %s", i, r.Action, r.Error)
		}
	}
}

func TestRunHTTPStep_ExpectFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprintln(w, "not found")
	})
	ln, port := listenOnFreePort(t)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	topo := &config.TopologyConfig{
		Services: map[string]config.ServiceConfig{
			"api": {
				Interfaces: map[string]config.InterfaceConfig{
					"public": {Protocol: "http", Port: port},
				},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	steps := []config.StepConfig{
		{Action: "api.get", Args: map[string]any{
			"path":   "/missing",
			"expect": map[string]any{"status": 200},
		}},
	}

	results, err := RunSteps(context.Background(), topo, steps, log)
	if err != nil {
		t.Fatalf("RunSteps: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Success {
		t.Fatal("expected step to fail (status 404 vs expected 200)")
	}
	if !strings.Contains(results[0].Error, "expected status 200, got 404") {
		t.Fatalf("unexpected error: %s", results[0].Error)
	}
}

func TestRunTCPStep(t *testing.T) {
	// Start a simple TCP echo server.
	ln, port := listenTCPFreePort(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				line := strings.TrimSpace(string(buf[:n]))
				if line == "PING" {
					fmt.Fprintln(c, "PONG")
				} else {
					fmt.Fprintln(c, "ERR")
				}
			}(conn)
		}
	}()
	defer ln.Close()

	topo := &config.TopologyConfig{
		Services: map[string]config.ServiceConfig{
			"db": {
				Interfaces: map[string]config.InterfaceConfig{
					"main": {Protocol: "tcp", Port: port},
				},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	steps := []config.StepConfig{
		{Action: "db.send", Args: map[string]any{
			"data":   "PING",
			"expect": map[string]any{"equals": "PONG"},
		}},
	}

	results, err := RunSteps(context.Background(), topo, steps, log)
	if err != nil {
		t.Fatalf("RunSteps: %v", err)
	}

	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success, got %+v", results)
	}
}

func TestParseStepAction(t *testing.T) {
	tests := []struct {
		input   string
		service string
		iface   string
		op      string
		wantErr bool
	}{
		{"api.get", "api", "", "get", false},
		{"api.public.post", "api", "public", "post", false},
		{"db.main.send", "db", "main", "send", false},
		{"bad", "", "", "", true},
		{"a.b.c.d", "", "", "", true},
	}
	for _, tt := range tests {
		svc, iface, op, err := parseStepAction(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseStepAction(%q) should fail", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStepAction(%q): %v", tt.input, err)
			continue
		}
		if svc != tt.service || iface != tt.iface || op != tt.op {
			t.Errorf("parseStepAction(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.input, svc, iface, op, tt.service, tt.iface, tt.op)
		}
	}
}

func TestSleepStep(t *testing.T) {
	topo := &config.TopologyConfig{
		Services: map[string]config.ServiceConfig{},
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dur := config.Duration{}
	dur.Duration = 50 * 1e6 // 50ms
	steps := []config.StepConfig{
		{Sleep: &dur},
	}

	results, err := RunSteps(context.Background(), topo, steps, log)
	if err != nil {
		t.Fatalf("RunSteps: %v", err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("expected success, got %+v", results)
	}
}

// --- helpers ---

func listenOnFreePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

func listenTCPFreePort(t *testing.T) (*net.TCPListener, int) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}
