package star

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/engine"
	"go.starlark.net/starlark"
)

func TestShutdownTimeoutNamesServiceAndPreventsFalsePass(t *testing.T) {
	rt := New(testLogger())
	rt.shutdownTimeout = 25 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	done := make(chan *engine.Result)
	close(done)
	rt.globals = starlark.StringDict{"test_leak": starlark.NewBuiltin("test_leak", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		rt.sessions["stuck-service"] = &runningSession{cancel: func() { <-release }, done: done}
		return starlark.None, nil
	})}
	start := time.Now()
	result := rt.RunTest(context.Background(), "test_leak")
	if time.Since(start) > time.Second {
		t.Fatal("teardown exceeded its bound")
	}
	if result.Result != "fail" || !strings.Contains(result.Reason, "stuck-service") || !strings.Contains(result.Reason, "cancel") {
		t.Fatalf("false success: %+v", result)
	}
	next := rt.RunTest(context.Background(), "test_leak")
	if next.Result != "fail" || !strings.Contains(next.Reason, "fresh runtime") {
		t.Fatalf("reused poisoned runtime: %+v", next)
	}
}

func TestSleepWorksInSeedAndSetup(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("startup.star", `
def seed(): sleep("1ms")
def setup(): sleep("1ms")
def body(): assert_true(True)
test("startup",body=body,setup=setup)
`); err != nil {
		t.Fatal(err)
	}
	end := rt.beginSpecOutput("test_startup")
	defer end()
	svc := &ServiceDef{Name: "seeded", Seed: rt.globals["seed"].(starlark.Callable)}
	if err := rt.runSeedCallback(svc.Name, svc); err != nil {
		t.Fatal(err)
	}
	rt.DiscoverTests()
	if result := rt.RunTest(context.Background(), "test_startup"); result.Result != "pass" {
		t.Fatal(result.Reason)
	}
}

func TestShutdownPanicIsReported(t *testing.T) {
	rt := New(testLogger())
	if rt.shutdownPhase("broken", "stop", func(context.Context) error { panic("broken stopper") }) {
		t.Fatal("panic accepted")
	}
	events := rt.events.Events()
	if len(events) != 1 || !strings.Contains(events[0].Fields["error"], "broken stopper") {
		t.Fatalf("missing panic evidence: %+v", events)
	}
}
