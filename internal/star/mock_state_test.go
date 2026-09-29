package star

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/protocol"
	"go.starlark.net/starlark"
)

func statefulGRPCRuntime(t *testing.T, handler, tests string) *Runtime {
	t.Helper()
	pb := writeTestDescriptorSet(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/grpc.star", "grpc")
%s
config = grpc.server(
    name = "config",
    interface = interface("main", "grpc", %d),
    descriptors = %q,
    state = {"users": {"42": "DENIED", "43": "APPROVED"}},
    services = {"/test.config.ConfigService/GetSetting": grpc.dynamic(answer)},
)
api = client("config-client", target = config.main, descriptors = %q)
%s
`, handler, freePort(t), pb, pb, tests)
	if err := rt.LoadString("stateful_grpc.star", src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.stopServices)
	return rt
}

func runMockTest(t *testing.T, rt *Runtime, name string) TestResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rt.RunTest(ctx, name)
}

const statefulAnswer = `
def answer(req):
    assert_eq(type(req["body"]), "dict")
    assert_eq(type(req["raw_body"]), "bytes")
    user = req["body"]["id"]
    return grpc.response({"id": user, "name": req["state"]["users"][user], "scope": str(req["state_revision"])})
`

func TestMockStateTypedRequestsSwitchAndReset(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, `
def test_change():
    assert_eq(api.get_setting(id = 42).data["name"], "DENIED")
    assert_eq(api.get_setting(id = 43).data["name"], "APPROVED")
    state = {"users": {"42": "APPROVED", "43": "DENIED", "9007199254740993": "EXACT"}}
    config.set_state(state)
    state["users"]["42"] = "CALLER_MUTATION"
    result = api.get_setting(id = 42)
    assert_eq(result.data["name"], "APPROVED")
    assert_eq(result.data["scope"], "1")
    assert_eq(api.get_setting(id = 43).data["name"], "DENIED")
    assert_eq(api.get_setting(id = 9007199254740993).data["name"], "EXACT")

def test_reset():
    result = api.get_setting(id = 42)
    assert_eq(result.data["name"], "DENIED")
    assert_eq(result.data["scope"], "0")
`)
	for _, name := range []string{"test_change", "test_reset", "test_change", "test_reset"} {
		tr := runMockTest(t, rt, name)
		if tr.Result != "pass" {
			t.Fatalf("%s: %s: %s", name, tr.Result, tr.Reason)
		}
		changes := 0
		for _, ev := range tr.Events {
			if ev.Type == "mock.state_changed" {
				changes++
				if ev.Fields["revision"] != "1" || !strings.Contains(ev.Fields["state"], "APPROVED") {
					t.Fatalf("bad state change event: %+v", ev)
				}
			}
		}
		if (name == "test_change" && changes != 1) || (name == "test_reset" && changes != 0) {
			t.Fatalf("%s: %d state changes", name, changes)
		}
	}
}

func TestMockErrorsCannotPassThroughFallback(t *testing.T) {
	for _, tc := range []struct{ name, handler, code string }{
		{"encode", `def answer(req):
    return grpc.response({"typo_field": "bad"})`, "MOCK_ENCODE_ERROR"},
		{"dynamic", `def answer(req):
    return grpc.response({"name": req["body"]["missing"]})`, "MOCK_DYNAMIC_ERROR"},
		{"frozen_state", `def answer(req):
    req["state"]["users"]["42"] = "BAD"
    return grpc.response({})`, "MOCK_DYNAMIC_ERROR"},
		{"handler_set_state", `def answer(req):
    config.set_state({})
    return grpc.response({})`, "MOCK_DYNAMIC_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := statefulGRPCRuntime(t, tc.handler, `
def test_fallback():
    result = api.get_setting(id = 42)
    assert_eq(result.ok, False)
    assert_true("Internal" in result.error)
`)
			tr := runMockTest(t, rt, "test_fallback")
			if tr.Result != "fail" || !strings.Contains(tr.Reason, tc.code) {
				t.Fatalf("bad verdict: %s: %s", tr.Result, tr.Reason)
			}
			out := BuildTraceOutput("stateful_grpc.star", &SuiteResult{Tests: []TestResult{tr}})
			found := false
			for _, d := range out.Tests[0].Diagnostics {
				if d.Code == tc.code && d.Service == "config" && d.Level == "error" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s diagnostic: %+v", tc.code, out.Tests[0].Diagnostics)
			}
		})
	}
}

func TestMockIntentionalErrorAndRecovery(t *testing.T) {
	rt := statefulGRPCRuntime(t, `
def answer(req):
    if req["state"].get("down", False):
        return grpc.unavailable("wd2 down")
    return grpc.response({"name": "APPROVED"})
`, `
def test_recovery():
    config.set_state({"down": True})
    result = api.get_setting(id = 42)
    assert_eq(result.ok, False)
    assert_true("Unavailable" in result.error)
    config.set_state({"down": False})
    assert_eq(api.get_setting(id = 42).data["name"], "APPROVED")
`)
	tr := runMockTest(t, rt, "test_recovery")
	if tr.Result != "pass" {
		t.Fatalf("%s: %s", tr.Result, tr.Reason)
	}
	for _, ev := range tr.Events {
		if d, ok := mockErrorDiagnostic(ev); ok {
			t.Fatalf("intentional error invalidated test: %+v", d)
		}
	}
}

func TestMockStateValidationAndSnapshots(t *testing.T) {
	state := starlark.NewDict(1)
	list := starlark.NewList([]starlark.Value{starlark.MakeInt64(9007199254740993)})
	_ = state.SetKey(starlark.String("values"), list)
	snapshot, err := freezeMockState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := list.SetIndex(0, starlark.MakeInt(1)); err != nil {
		t.Fatal(err)
	}
	values, _, _ := snapshot.Get(starlark.String("values"))
	if values.(*starlark.List).Index(0).String() != "9007199254740993" {
		t.Fatal("snapshot changed")
	}
	if err := values.(*starlark.List).SetIndex(0, starlark.MakeInt(2)); err == nil {
		t.Fatal("snapshot is mutable")
	}
	_ = state.SetKey(starlark.String("cycle"), state)
	if _, err := freezeMockState(state); err == nil {
		t.Fatal("accepted cyclic state")
	}

	rt := New(testLogger())
	if err := rt.LoadString("invalid.star", `
m = mock_service("m", interface("main", "http", 12345))
m.set_state({})
`); err == nil || !strings.Contains(err.Error(), "only available in a test body") {
		t.Fatalf("load-time mutation: %v", err)
	}
}

func TestMockStateReplacementDoesNotChangeInFlightHandler(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, "")
	svc := rt.services["config"]
	svc.Mock.resetState()
	rt.inTest.Store(true)
	defer rt.inTest.Store(false)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var first atomic.Bool
	fn := starlark.NewBuiltin("answer", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		req := args[0].(*starlark.Dict)
		if !first.Swap(true) {
			close(entered)
			<-release
		}
		state, _, _ := req.Get(starlark.String("state"))
		data, err := marshalJSONBody(state)
		return newStaticResponse(&protocol.MockResponse{Body: data}), err
	})
	handler := rt.dynamicHandlerBridge("config", "main", "/test.config.ConfigService/GetSetting", fn)
	type response struct {
		value *protocol.MockResponse
		err   error
	}
	done := make(chan response, 1)
	go func() { value, err := handler(protocol.MockRequest{}); done <- response{value, err} }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	state := starlark.NewDict(1)
	_ = state.SetKey(starlark.String("phase"), starlark.String("new"))
	setter, err := svc.Attr("set_state")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := starlark.Call(&starlark.Thread{Name: "test_change"}, setter.(starlark.Callable), starlark.Tuple{state}, nil); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !strings.Contains(string(result.value.Body), "DENIED") {
			t.Fatalf("in-flight handler saw new state: %s", result.value.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return")
	}
	result, err := handler(protocol.MockRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["phase"] != "new" {
		t.Fatalf("next handler did not see replacement: %s", result.Body)
	}
}

// Customer specs share upstreams through several transitive load() paths.
// Runtime service operations resolve by name; set_state must do the same.
func TestMockStateThroughTransitiveLoads(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"upstream.star": fmt.Sprintf(`
m = mock_service("m", interface("main", "http", %d),
    state = {"value": "old"},
    routes = {"GET /": dynamic(lambda req: json_response(200, req["state"]))})
`, freePort(t)),
		"app.star": "load(\"upstream.star\", \"m\")\nother_m = m\n",
		"test.star": `
load("upstream.star", "m")
load("app.star", other = "other_m")
def test_shared_state():
    m.set_state({"value": "new"})
    assert_eq(m.main.get(path = "/").data["value"], "new")
    other.set_state({"value": "newer"})
    assert_eq(m.main.get(path = "/").data["value"], "newer")
`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rt := New(testLogger())
	if err := rt.LoadFile(filepath.Join(dir, "test.star")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.stopServices)
	tr := runMockTest(t, rt, "test_shared_state")
	if tr.Result != "pass" {
		t.Fatalf("%s: %s", tr.Result, tr.Reason)
	}
}
