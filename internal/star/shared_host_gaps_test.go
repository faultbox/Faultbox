package star

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.starlark.net/starlark"
)

func TestEventsCombinesFiltersBeforePredicate(t *testing.T) {
	rt := New(testLogger())
	rt.events.Emit("custom.payload", "other", map[string]string{"data": `{"wrong": true}`})
	rt.events.Emit("custom.payload", "wanted", map[string]string{"data": `{"ok": true}`})
	v, err := starlark.Eval(&starlark.Thread{}, "filters.star", `events(service="wanted", where=lambda e: e.data["ok"])`, rt.builtins())
	if err != nil || v.(*starlark.List).Len() != 1 {
		t.Fatalf("filters not ANDed before callback: %v %v", v, err)
	}
}

func TestRemoteSeedUsesTestParamsBeforeDependents(t *testing.T) {
	var mu sync.Mutex
	stored := ""
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			stored = string(b)
		}
		if r.URL.Path == "/sut-ready" {
			seen = append(seen, stored)
		}
		w.Header().Set("X-Test", "header-value")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	rt := New(testLogger())
	src := fmt.Sprintf(`
def seed():
    info = current_test()
    assert_true(info.name.startswith("test_"))
    r = db.main.post(body=json.encode(info.params))
    assert_true(r.ok)
    assert_eq(r.headers["X-Test"], "header-value")
    assert_eq(r.header_values["X-Test"], ["header-value"])
    emit("seed", info.params)
db = service("db", interface("main", "http", 1, proxy=False), remote=%q,
    healthcheck=http(%q), seed=seed)
sut = service("sut", interface("main", "http", 2, proxy=False), remote=%q,
    depends_on=[db], healthcheck=http(%q))
def body():
    assert_true(current_test().params["flag"] in [True,False])
test("one", params={"flag":True}, body=body)
test("two", params={"flag":False}, body=body)
`, strings.TrimPrefix(srv.URL, "http://"), srv.URL, strings.TrimPrefix(srv.URL, "http://"), srv.URL+"/sut-ready")
	if err := rt.LoadString("params.star", src); err != nil {
		t.Fatal(err)
	}
	rt.DiscoverTests()
	for _, name := range []string{"test_one", "test_two"} {
		r := rt.RunTest(context.Background(), name)
		if r.Result != "pass" {
			t.Fatalf("%s: %s", name, r.Reason)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != `{"flag":true}` || seen[1] != `{"flag":false}` {
		t.Fatalf("dependent saw stale config: %v", seen)
	}
}

func TestPortWaitIgnoresRemoteAndRunningServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	var port int
	fmt.Sscanf(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"), "%d", &port)
	rt := New(testLogger())
	rt.services["remote"] = &ServiceDef{Remote: "127.0.0.1", Interfaces: map[string]*InterfaceDef{"http": {Port: port}}}
	rt.services["reused"] = &ServiceDef{Reuse: true, Interfaces: map[string]*InterfaceDef{"http": {Port: port}}}
	rt.sessions["reused"] = &runningSession{}
	start := time.Now()
	rt.waitPortsFree(time.Second)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("waited for externally owned/running ports")
	}
}

func TestProxyOptOutRejectsFaultAndAddress(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("direct.star", `
db=service("db", interface("main","mysql",3306,proxy=False),remote="127.0.0.2",healthcheck=tcp("127.0.0.2:3306"))
`); err != nil {
		t.Fatal(err)
	}
	ref := &InterfaceRef{Service: rt.services["db"], Interface: rt.services["db"].Interfaces["main"], runtime: rt}
	if _, err := ref.Attr("proxy_addr"); err == nil {
		t.Fatal("disabled interface exposed proxy placeholder")
	}
	_, err := rt.builtinFaultProtocol(&starlark.Thread{}, ref, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("fault should reject opted-out proxy: %v", err)
	}
}

func TestDecompressBinaryAndLimits(t *testing.T) {
	original := []byte{0x80, 0xff, 0, 1, 2, 3}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(original)
	w.Close()
	z, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	zw := z.EncodeAll(original, nil)
	z.Close()
	for format, wire := range map[string][]byte{"gzip": gz.Bytes(), "zstd": zw} {
		rt := New(testLogger())
		globals := rt.builtins()
		globals["encoded"] = starlark.String(base64.StdEncoding.EncodeToString(wire))
		v, err := starlark.Eval(&starlark.Thread{}, "decompress.star", fmt.Sprintf(`decompress(format=%q,data_base64=encoded)`, format), globals)
		if err != nil || !bytes.Equal([]byte(v.(starlark.Bytes)), original) {
			t.Fatalf("%s: %v %v", format, v, err)
		}
		_, err = starlark.Eval(&starlark.Thread{}, "decompress.star", fmt.Sprintf(`decompress(format=%q,data_base64=encoded,max_output_bytes=2)`, format), globals)
		if err == nil {
			t.Fatal("oversized decompression accepted")
		}
	}
}
