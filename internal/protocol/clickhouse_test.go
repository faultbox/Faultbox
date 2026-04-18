package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClickhouseProtocolRegistered(t *testing.T) {
	p, ok := Get("clickhouse")
	if !ok {
		t.Fatal("clickhouse protocol not registered")
	}
	want := []string{"query", "exec"}
	if !reflect.DeepEqual(p.Methods(), want) {
		t.Errorf("Methods() = %v, want %v", p.Methods(), want)
	}
}

// TestClickhouse_Query exercises the SELECT path end-to-end against a
// stubbed ClickHouse HTTP endpoint. Verifies the plugin appends `FORMAT
// JSON`, POSTs SQL as the body, and parses the response into data rows.
func TestClickhouse_Query(t *testing.T) {
	var receivedSQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedSQL = string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"meta":[{"name":"n"}],"data":[{"n":42}],"rows":1}`)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	p := &clickhouseProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "query", map[string]any{"sql": "SELECT count() as n FROM events"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !res.Success {
		t.Fatalf("step failed: %s", res.Error)
	}
	if !strings.Contains(receivedSQL, "FORMAT JSON") {
		t.Errorf("sql sent to server = %q, missing FORMAT JSON", receivedSQL)
	}

	var data []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &data); err != nil {
		t.Fatalf("parse result body: %v (raw: %s)", err, res.Body)
	}
	if len(data) != 1 || data[0]["n"] != float64(42) {
		t.Errorf("data = %v, want [{n:42}]", data)
	}
	if res.Fields["rows"] != "1" {
		t.Errorf("rows field = %q, want 1", res.Fields["rows"])
	}
}

func TestClickhouse_Exec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	p := &clickhouseProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "exec", map[string]any{"sql": "CREATE TABLE t (id Int64) ENGINE=MergeTree ORDER BY id"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !res.Success {
		t.Errorf("step failed: %s", res.Error)
	}
	if !strings.Contains(res.Body, `"ok":true`) {
		t.Errorf("body = %q, want {ok:true}", res.Body)
	}
}

func TestClickhouse_QueryErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "Code: 60. DB::Exception: Table events doesn't exist")
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	p := &clickhouseProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := p.ExecuteStep(ctx, addr, "query", map[string]any{"sql": "SELECT * FROM events"})
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if res.Success {
		t.Error("expected failure for 500 response")
	}
	if !strings.Contains(res.Error, "Table events doesn't exist") {
		t.Errorf("error = %q, missing ClickHouse exception text", res.Error)
	}
	if res.StatusCode != 500 {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

func TestClickhouse_MissingSQL(t *testing.T) {
	p := &clickhouseProtocol{}
	_, err := p.ExecuteStep(context.Background(), "127.0.0.1:1", "query", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "sql=") {
		t.Errorf("expected sql= required error, got %v", err)
	}
}

func TestClickhouse_UnsupportedMethod(t *testing.T) {
	p := &clickhouseProtocol{}
	_, err := p.ExecuteStep(context.Background(), "127.0.0.1:1", "drop_table", map[string]any{"sql": "x"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected unsupported method error, got %v", err)
	}
}
