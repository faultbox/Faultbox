package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExtractClickhouseSQL_FromBody(t *testing.T) {
	body := bytes.NewReader([]byte("SELECT 1"))
	req, _ := http.NewRequest("POST", "http://example/", body)
	got := extractClickhouseSQL(req)
	if got != "SELECT 1" {
		t.Errorf("got %q, want SELECT 1", got)
	}
	// Body must still be readable after extraction — the reverse proxy
	// needs it to forward upstream.
	after, _ := io.ReadAll(req.Body)
	if string(after) != "SELECT 1" {
		t.Errorf("body not restored: %q", after)
	}
}

func TestExtractClickhouseSQL_FromURL(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://example/?query=SELECT%202", nil)
	got := extractClickhouseSQL(req)
	if got != "SELECT 2" {
		t.Errorf("got %q, want SELECT 2", got)
	}
}

func TestClickhouseProxy_Protocol(t *testing.T) {
	p, err := newProxy("clickhouse", nil, "test")
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	if p.Protocol() != "clickhouse" {
		t.Errorf("Protocol() = %q", p.Protocol())
	}
}

// TestClickhouseProxy_InjectsError verifies the proxy returns a ClickHouse-
// shaped error response (text/plain, "Code: N. DB::Exception: ...") when
// a query matches an error rule.
func TestClickhouseProxy_InjectsError(t *testing.T) {
	// Unreachable upstream — rule must fire before forwarding.
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	p := newClickhouseProxy(nil, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	target := upstream.URL[len("http://"):]
	addr, err := p.Start(ctx, target)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	p.AddRule(Rule{
		Action: ActionError,
		Query:  "SELECT*",
		Error:  "too many parts",
	})

	resp, err := http.Post("http://"+addr+"/", "text/plain", bytes.NewReader([]byte("SELECT count() FROM events")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("DB::Exception: too many parts")) {
		t.Errorf("body = %q, missing ClickHouse exception shape", body)
	}
	if upstreamHit {
		t.Error("upstream should NOT be hit when error rule fires")
	}
}
