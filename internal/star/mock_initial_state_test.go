package star

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faultbox/Faultbox/internal/protocol"
	"go.starlark.net/starlark"
)

func TestMockStateAppliedBeforeDependentBoot(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, fmt.Sprintf(`
consumer=mock_service("consumer",interface("main","http",%d),depends_on=[config],routes={"GET /":text_response(200,"pending")})
def check_off():
    assert_eq(consumer.main.get(path="/").data["cached"],"DENIED")
def check_on():
    assert_eq(consumer.main.get(path="/").data["cached"],"APPROVED")
    assert_eq(api.get_setting(id=42).data["scope"],"0")
    config.set_state({"users":{"42":"CHANGED_AFTER_BOOT"}})
    assert_eq(consumer.main.get(path="/").data["cached"],"APPROVED")
test("off",body=check_off)
test("on",body=check_on,mock_state={config.name:{"users":{"42":"APPROVED"}}})
`, freePort(t)))
	var cached atomic.Value
	cached.Store("pending")
	svc := rt.services["consumer"]
	svc.Mock.Routes["main"][0].Response = newDynamicResponse(starlark.NewBuiltin("cached", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		data, _ := json.Marshal(map[string]any{"cached": cached.Load()})
		return newStaticResponse(&protocol.MockResponse{Status: 200, Body: data}), nil
	}))
	// This dependency's startup callback makes a real gRPC request once and
	// caches it, like a SUT loading feature toggles before readiness.
	svc.Seed = starlark.NewBuiltin("boot", func(thread *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		client, _ := rt.Client("config-client")
		operation, _ := client.Table.Lookup("get_setting")
		value, err := rt.executeClientCall(thread, client, operation, map[string]any{"id": int64(42)})
		if err != nil {
			return nil, err
		}
		resp := value.(*Response)
		if !resp.Ok {
			return nil, fmt.Errorf("boot request: %s", resp.Error)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(resp.Body), &data); err != nil {
			return nil, err
		}
		cached.Store(data["name"].(string))
		return starlark.None, nil
	})
	for _, name := range []string{"test_off", "test_on", "test_off", "test_on"} {
		rt.DiscoverTests()
		tr := runMockTest(t, rt, name)
		if tr.Result != "pass" {
			t.Fatalf("%s: %s", name, tr.Reason)
		}
		if name == "test_on" {
			initial, started := int64(0), int64(0)
			for _, ev := range tr.Events {
				if ev.Type == "mock.state_initialized" && ev.Service == "config" {
					initial = ev.Seq
				}
				if ev.Type == "service_started" && ev.Service == "consumer" {
					started = ev.Seq
				}
			}
			if initial == 0 || started <= initial {
				t.Fatalf("state initialized %d after dependent start %d", initial, started)
			}
		}
	}
}

func TestMockInitialStateResetForEachLeaf(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, `
def body():
    assert_eq(api.get_setting(id=42).data["name"],"APPROVED")
    config.set_state({"users":{"42":"MUTATED"}})
test("leaf",body=body,mock_state={"config":{"users":{"42":"APPROVED"}}})
`)
	rt.DiscoverTests()
	for i := 1; i <= 2; i++ {
		tr := rt.RunTestLeaf(context.Background(), "test_leaf", &PlanLeaf{Index: i})
		if tr.Result != "pass" {
			t.Fatal(tr.Reason)
		}
	}
}

func TestMockInitialStateRejectsInvalidTargetsAndReuse(t *testing.T) {
	for _, tc := range []struct{ name, extra, states, want string }{
		{"missing", "", `{"typo":{}}`, "not a registered mock"},
		{"real", `real=service("real",binary="/bin/true")`, `{"real":{}}`, "not a registered mock"},
		{"reuse", `middle=mock_service("middle",interface("main","http",19001),depends_on=[config])
real=service("real",image="unused",reuse=True,depends_on=[middle])`, `{"config":{}}`, "reused dependent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := statefulGRPCRuntime(t, statefulAnswer, tc.extra+"\ntest(\"invalid\",body=lambda:None,mock_state="+tc.states+")")
			rt.DiscoverTests()
			tr := runMockTest(t, rt, "test_invalid")
			if tr.Result != "fail" || !strings.Contains(tr.Reason, tc.want) {
				t.Fatalf("%s: %s", tr.Result, tr.Reason)
			}
			for _, ev := range tr.Events {
				if ev.Type == "service_started" {
					t.Fatal("started services before validation")
				}
			}
		})
	}
}
