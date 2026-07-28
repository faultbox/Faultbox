package star

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// Spec-load integration for the packet DSL: runtime gating, the determinism
// ceiling, and — most importantly — that none of this disturbed the existing
// delay()/drop() overloads.

// evalIn evaluates one expression against a runtime's builtins.
func evalIn(t *testing.T, rt *Runtime, expr string) (starlark.Value, error) {
	t.Helper()
	return starlark.Eval(&starlark.Thread{Name: "test"}, "test.star", expr, rt.builtins())
}

func TestRuntimeGVisorAccepted(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", `determinism(runtime = "gvisor")`); err != nil {
		t.Fatalf("runtime=\"gvisor\" rejected: %v", err)
	}
	if rt.detRuntime != DeterminismRuntimeGVisor {
		t.Errorf("detRuntime = %q, want %q", rt.detRuntime, DeterminismRuntimeGVisor)
	}
}

// TestRuntimeGVisorNetNoLongerExists pins the collapse back to RFC-046's two
// values. An earlier RFC-054 draft added "gvisor-net" so a packet-faults-only
// spec would not drag runsc along; the runsc requirement is now driven by
// whether the spec calls watch(), which achieves the same thing without a
// second runtime name.
func TestRuntimeGVisorNetNoLongerExists(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `determinism(runtime = "gvisor-net")`)
	if err == nil {
		t.Fatal("runtime=\"gvisor-net\" was accepted; there are only two runtimes")
	}
}

func TestRuntimeUnknownRejected(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `determinism(runtime = "firecracker")`)
	if err == nil {
		t.Fatal("unknown runtime accepted")
	}
	for _, want := range []string{"default", "gvisor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q as valid, got: %v", want, err)
		}
	}
}

// TestDeterminismL2StillErrors is the anti-overclaim guard. RFC-054 widens the
// mediated surface but does not raise the ceiling: L2 means *total* event
// determinism including clock and RNG, which neither gVisor path delivers.
// Conflating "we see more" with "we promise more" is exactly what RFC-040 was
// written to stop.
func TestDeterminismL2StillErrors(t *testing.T) {
	for _, level := range []string{"L2", "L3", "L4", "L5"} {
		t.Run(level, func(t *testing.T) {
			rt := New(testLogger())
			err := rt.LoadString("test.star", `determinism(level = "`+level+`", runtime = "gvisor")`)
			if err == nil {
				t.Fatalf("determinism(level=%q) was accepted; the ceiling is still L1", level)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error = %v, want it to say the level is reserved", err)
			}
		})
	}
}

// TestPacketFaultRequiresGvisorRuntime: installing a packet fault under the
// default runtime would inject nothing and the test would pass, which is worse
// than refusing to load.
func TestPacketFaultRequiresGvisorRuntime(t *testing.T) {
	rt := New(testLogger())
	spec := `
db = service("db", "/bin/true", interface("main", "tcp", 5432))

def test_packets():
    fault(db.main, packet_drop(), run = lambda: None)
`
	if err := rt.LoadString("test.star", spec); err != nil {
		t.Fatalf("spec load: %v", err)
	}
	// The error surfaces when the fault window runs.
	res := rt.RunTest(context.Background(), "test_packets")
	if res.Result != "fail" {
		t.Fatalf("test result = %q, want fail", res.Result)
	}
	if !strings.Contains(res.Reason, "runtime=") {
		t.Errorf("reason should name the runtime problem, got: %s", res.Reason)
	}
	if !strings.Contains(res.Reason, "gvisor") {
		t.Errorf("reason should name the fix (runtime=gvisor), got: %s", res.Reason)
	}
}

// ─── the highest-risk regression in the release ────────────────────────────

// TestDelayOverloadUnaffected pins that adding packet_delay did not disturb
// the two existing meanings of delay(). The docs carry an explicit warning
// about this overload and v0.13.2/0.13.3 shipped fixes for kwargs landing in
// the wrong one, so it gets its own test.
func TestDelayOverloadUnaffected(t *testing.T) {
	rt := New(testLogger())
	// Syscall level: positional duration → FaultDef.
	v, err := evalIn(t, rt, `delay("500ms")`)
	if err != nil {
		t.Fatalf(`delay("500ms"): %v`, err)
	}
	fd, ok := v.(*FaultDef)
	if !ok {
		t.Fatalf(`delay("500ms") returned %s, want fault (syscall level)`, v.Type())
	}
	if fd.Delay != 500*time.Millisecond {
		t.Errorf("Delay = %v, want 500ms", fd.Delay)
	}

	// Protocol level: kwargs only → ProxyFaultDef.
	v, err = evalIn(t, rt, `delay(path = "/data/*", delay = "500ms")`)
	if err != nil {
		t.Fatalf("delay(path=...): %v", err)
	}
	pfd, ok := v.(*ProxyFaultDef)
	if !ok {
		t.Fatalf("delay(path=...) returned %s, want proxy_fault", v.Type())
	}
	if pfd.Path != "/data/*" || pfd.Delay != 500*time.Millisecond {
		t.Errorf("proxy delay not parsed: %+v", pfd)
	}

	// Packet level is a *different name*, so it cannot collide.
	v, err = evalIn(t, rt, `packet_delay("500ms")`)
	if err != nil {
		t.Fatalf(`packet_delay("500ms"): %v`, err)
	}
	if _, ok := v.(*PacketFaultDef); !ok {
		t.Fatalf(`packet_delay returned %s, want packet_fault`, v.Type())
	}
}

func TestDropOverloadUnaffected(t *testing.T) {
	rt := New(testLogger())
	v, err := evalIn(t, rt, `drop(query = "INSERT*")`)
	if err != nil {
		t.Fatalf("drop(query=...): %v", err)
	}
	pfd, ok := v.(*ProxyFaultDef)
	if !ok {
		t.Fatalf("drop(query=...) returned %s, want proxy_fault", v.Type())
	}
	if pfd.Query != "INSERT*" {
		t.Errorf("Query = %q", pfd.Query)
	}
}

// ─── registry & unwired-gateway behaviour ──────────────────────────────────

type stubGateway struct {
	installed map[string][]*netfault.Rule
	cleared   int
}

func newStubGateway() *stubGateway {
	return &stubGateway{installed: make(map[string][]*netfault.Rule)}
}

func (g *stubGateway) InstallRules(consumer, service, iface string, rules []*netfault.Rule) error {
	g.installed[stubKey(consumer, service, iface)] = rules
	return nil
}

func (g *stubGateway) ClearRules(consumer, service, iface string) error {
	delete(g.installed, stubKey(consumer, service, iface))
	g.cleared++
	return nil
}

func stubKey(consumer, service, iface string) string {
	if consumer == "" {
		return service + "." + iface
	}
	return consumer + "->" + service + "." + iface
}

func TestPacketRegistryInstallsToGateway(t *testing.T) {
	reg := newPacketRuleRegistry()
	g := newStubGateway()
	reg.SetGateway(g)

	rules := []*netfault.Rule{{Action: netfault.ActionDrop}}
	if err := reg.install("", "db", "main", rules); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(g.installed["db.main"]) != 1 {
		t.Errorf("gateway got %d rules, want 1", len(g.installed["db.main"]))
	}
	if reg.unwiredInstalls() != 0 {
		t.Errorf("unwiredInstalls = %d, want 0 with a gateway attached", reg.unwiredInstalls())
	}

	if err := reg.clear("", "db", "main"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(g.installed) != 0 {
		t.Error("rules were not cleared from the gateway")
	}
	if g.cleared != 1 {
		t.Errorf("ClearRules called %d times, want 1", g.cleared)
	}
}

// TestPacketRegistryCountsUnwiredInstalls: with no gateway, an install must be
// recorded so the runtime can fail the test rather than report a pass for a
// fault that touched nothing.
func TestPacketRegistryCountsUnwiredInstalls(t *testing.T) {
	reg := newPacketRuleRegistry()
	if err := reg.install("", "db", "main", []*netfault.Rule{{Action: netfault.ActionDrop}}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := reg.unwiredInstalls(); got != 1 {
		t.Errorf("unwiredInstalls = %d, want 1", got)
	}
}

// TestEventsWhereSeesAllEventTypes is a regression guard for a pre-existing
// bug found while building the RFC-054 scenario corpus: events() filtered to
// {syscall, stdout, topic, wal} *before* invoking the where= lambda, so a
// predicate naming any other family silently matched nothing.
//
// docs/spec-language.md has always documented
// `events(where=lambda e: e.type == "proxy" ...)`, which could never have
// worked. unmediated_io (RFC-040) and packet (RFC-054) had the same problem.
func TestEventsWhereSeesAllEventTypes(t *testing.T) {
	rt := New(testLogger())
	rt.events.Emit("packet", "db", map[string]string{"action": "drop"})
	rt.events.Emit("proxy", "db", map[string]string{"action": "error"})
	rt.events.Emit("unmediated_io", "api", map[string]string{"category": "clock"})
	rt.events.Emit("syscall", "db", map[string]string{"syscall": "write"})

	for _, want := range []string{"packet", "proxy", "unmediated_io", "syscall"} {
		src := `events(where = lambda e: e.type == "` + want + `")`
		v, err := evalIn(t, rt, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		list, ok := v.(*starlark.List)
		if !ok {
			t.Fatalf("%s returned %s, want a list", src, v.Type())
		}
		if list.Len() == 0 {
			t.Errorf("events(where=e.type==%q) found nothing; the where= lambda is being pre-filtered", want)
		}
	}
}

// TestEventsDictFilterKeepsAllowList pins that the fix did not widen the
// dict-filter path, where syscall=/path=/decision= only make sense for the
// historical families.
func TestEventsDictFilterKeepsAllowList(t *testing.T) {
	rt := New(testLogger())
	rt.events.Emit("packet", "db", map[string]string{"action": "drop"})
	rt.events.Emit("syscall", "db", map[string]string{"syscall": "write"})

	v, err := evalIn(t, rt, `events(service = "db")`)
	if err != nil {
		t.Fatalf("events(service=): %v", err)
	}
	list := v.(*starlark.List)
	for i := 0; i < list.Len(); i++ {
		se := list.Index(i).(*StarlarkEvent)
		if se.ev.Type == "packet" {
			t.Error("dict-filter path now returns packet events; that changes existing spec behaviour")
		}
	}
}
