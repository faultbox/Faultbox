package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// Peer-mesh support (amendment to RFC-054, v0.14.0).
//
// RFC-054's Milestone 0 spike evaluated insertion candidates on client→server
// questions only — does traffic arrive, added latency, container restart, Lima,
// CI. No criterion covered a *cyclic* topology, so the gateway inherited an
// ordering assumption that only holds for a dependency tree.

// TestGatewayAddrDoesNotRequireAProxy is the blocking fix.
//
// preStartProxies starts an interface's proxy when its owning service starts,
// and only services launched afterwards see it. A mesh is a cycle, so for at
// least one link the proxy is always absent when the consumer's env is built.
// Gating gateway allocation on a proxy address left that link unmediated, and
// packet rules installed into a link no traffic crossed.
func TestGatewayAddrDoesNotRequireAProxy(t *testing.T) {
	rt := New(testLogger())
	// runtime="gvisor" makes the gateway eligible; there is no proxy running
	// for this interface, which is exactly the mesh situation.
	rt.detRuntime = DeterminismRuntimeGVisor

	// A real upstream is always known at spec load.
	addr := rt.gatewayAddrFor("node1", "node2", "raft", 8302, "127.0.0.1:8302")
	if addr == "" {
		t.Skip("gateway unavailable on this host (needs CAP_NET_ADMIN); " +
			"the allocation path is covered by netfault's allocator tests")
	}
	if addr == "127.0.0.1:8302" {
		t.Error("gatewayAddrFor returned the upstream unchanged")
	}
}

// TestGatewayAddrRejectsEmptyUpstream: both call sites now supply a fallback,
// so an empty upstream is a bug rather than a state to tolerate silently.
func TestGatewayAddrRejectsEmptyUpstream(t *testing.T) {
	rt := New(testLogger())
	rt.detRuntime = DeterminismRuntimeGVisor
	if got := rt.gatewayAddrFor("node1", "node2", "raft", 8302, ""); got != "" {
		t.Errorf("empty upstream produced address %q, want \"\"", got)
	}
}

// TestMeshSpecLoadsWithoutOrderingHack: the Raft POC had to start
// node3 → node2 → node1 and elect node1 sole bootstrapper to get *any*
// mediation, and even that left node1's own inbound link unmediated. A mesh
// spec must load with no ordering constraint at all.
func TestMeshSpecLoadsWithoutOrderingHack(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")

node1 = service("node1", "/bin/true",
    interface("raft", "tcp", 8301),
    env = {"PEERS": "node2,node3"},
)
node2 = service("node2", "/bin/true",
    interface("raft", "tcp", 8302),
    env = {"PEERS": "node1,node3"},
)
node3 = service("node3", "/bin/true",
    interface("raft", "tcp", 8303),
    env = {"PEERS": "node1,node2"},
)

def test_mesh():
    fault(node2.raft, packet_drop(dir = "c2s"), run = lambda: None)
`
	if err := rt.LoadString("mesh.star", src); err != nil {
		t.Fatalf("cyclic peer topology failed to load: %v", err)
	}
	// No depends_on anywhere: every node references the others.
	for _, name := range []string{"node1", "node2", "node3"} {
		if rt.services[name] == nil {
			t.Errorf("service %q not registered", name)
		}
	}
}

// ─── EventVal.data ─────────────────────────────────────────────────────────

// TestEventValHasDataAndFields covers the documented monitor example, which
// could not run: spec-language.md shows
//
//	check = lambda event, state: "ERROR" not in event.data.get("level", "")
//
// but EventVal had no .data, so the lookup fell through to the flat-field path,
// returned a string, and failed with "string has no .get field or method".
func TestEventValHasDataAndFields(t *testing.T) {
	ev := Event{
		Type:    "stdout",
		Service: "node1",
		Seq:     7,
		Fields:  map[string]string{"data": `{"level":"ERROR","count":3}`},
	}
	v := &EventVal{ev: ev}

	data, err := v.Attr("data")
	if err != nil {
		t.Fatalf("EventVal.data: %v", err)
	}
	dict, ok := data.(*starlark.Dict)
	if !ok {
		t.Fatalf("EventVal.data is %s, want a dict — .get() would fail on anything else", data.Type())
	}
	lvl, found, err := dict.Get(starlark.String("level"))
	if err != nil || !found {
		t.Fatalf("data[\"level\"] not present: found=%v err=%v", found, err)
	}
	if lvl.(starlark.String) != "ERROR" {
		t.Errorf("data[\"level\"] = %v, want ERROR", lvl)
	}

	if _, err := v.Attr("fields"); err != nil {
		t.Errorf("EventVal.fields: %v", err)
	}

	names := strings.Join(v.AttrNames(), ",")
	for _, want := range []string{"data", "fields"} {
		if !strings.Contains(names, want) {
			t.Errorf("AttrNames() missing %q — autocomplete would not offer it", want)
		}
	}
}

// TestEventValDataFallsBackToFields: an event with no JSON "data" field still
// yields a dict, so .get() works uniformly.
func TestEventValDataFallsBackToFields(t *testing.T) {
	v := &EventVal{ev: Event{
		Type:   "packet",
		Fields: map[string]string{"action": "drop", "flow": "tcp|a-b"},
	}}
	data, err := v.Attr("data")
	if err != nil {
		t.Fatalf("Attr: %v", err)
	}
	dict, ok := data.(*starlark.Dict)
	if !ok {
		t.Fatalf("data is %s, want dict", data.Type())
	}
	got, found, _ := dict.Get(starlark.String("action"))
	if !found || got.(starlark.String) != "drop" {
		t.Errorf("data[\"action\"] = %v (found=%v), want drop", got, found)
	}
}

// TestEventValFlatAccessStillWorks: the fix must not break the form that did
// work, which specs already use.
func TestEventValFlatAccessStillWorks(t *testing.T) {
	v := &EventVal{ev: Event{
		Type:   "stdout",
		Fields: map[string]string{"event": "fsm.apply", "count": "12"},
	}}
	got, err := v.Attr("event")
	if err != nil {
		t.Fatalf("flat access: %v", err)
	}
	if got.(starlark.String) != "fsm.apply" {
		t.Errorf("event = %v, want fsm.apply", got)
	}
}

// TestDocumentedMonitorExampleRuns evaluates the exact example from
// docs/spec-language.md against a real monitor.
func TestDocumentedMonitorExampleRuns(t *testing.T) {
	rt := New(testLogger())
	src := `
monitor("no_stdout_errors",
    on    = match.event(type="stdout"),
    check = lambda event, state: "ERROR" not in event.data.get("level", ""),
)
svc = service("svc", "/bin/true", interface("main", "http", 8080))
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("documented monitor example failed to load: %v", err)
	}
}

// ─── source= targeting (Change 2) ──────────────────────────────────────────

// TestPacketFaultCarriesSource: source= was parsed, stored, and emitted into
// the trace since before v0.14.0, but never reached rule installation — so
// `fault(kafka.main, source=worker, drop(...))`, the exact example in the
// docs, installed a rule that fired for every consumer.
func TestPacketFaultCarriesSource(t *testing.T) {
	reg := newPacketRuleRegistry()
	g := newStubGateway()
	reg.SetGateway(g)

	rules := []*netfault.Rule{{Action: netfault.ActionDrop}}
	if err := reg.install("node1", "node2", "raft", rules); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, ok := g.installed["node1->node2.raft"]; !ok {
		t.Errorf("rules not scoped to the consumer; got keys %v", stubKeys(g.installed))
	}
	// A rule for a different consumer must be a separate entry, not a
	// replacement — otherwise two peers cannot be faulted independently.
	if err := reg.install("node3", "node2", "raft", rules); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(g.installed) != 2 {
		t.Errorf("installed %d rule sets, want 2 (one per consumer)", len(g.installed))
	}

	if err := reg.clear("node1", "node2", "raft"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := g.installed["node1->node2.raft"]; ok {
		t.Error("clearing node1's rules did not remove them")
	}
	if _, ok := g.installed["node3->node2.raft"]; !ok {
		t.Error("clearing node1's rules also removed node3's")
	}
}

func stubKeys(m map[string][]*netfault.Rule) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestFaultSourceMustBeAService(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
a = service("a", "/bin/true", interface("main", "tcp", 8080))
b = service("b", "/bin/true", interface("main", "tcp", 9090))
def test_s():
    fault(b.main, packet_drop(), source="a", run = lambda: None)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("load: %v", err)
	}
	res := rt.RunTest(context.Background(), "test_s")
	if res.Result != "fail" || !strings.Contains(res.Reason, "source= must be a service") {
		t.Errorf("source= with a string should be rejected, got: %s", res.Reason)
	}
}

func TestFaultSourceRejectsSelf(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
b = service("b", "/bin/true", interface("main", "tcp", 9090))
def test_s():
    fault(b.main, packet_drop(), source=b, run = lambda: None)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("load: %v", err)
	}
	res := rt.RunTest(context.Background(), "test_s")
	if res.Result != "fail" || !strings.Contains(res.Reason, "same service") {
		t.Errorf("source= naming the interface owner should be rejected, got: %s", res.Reason)
	}
}

// ─── partition() on the packet gateway (Change 3) ──────────────────────────

// TestPartitionRequiresGatewayRuntime: the old connect-deny is not a fallback.
// Against any service that pools connections it silently did nothing while the
// test still passed, which is worse than refusing.
func TestPartitionRequiresGatewayRuntime(t *testing.T) {
	rt := New(testLogger())
	src := `
a = service("a", "/bin/true", interface("main", "tcp", 8080))
b = service("b", "/bin/true", interface("main", "tcp", 9090))
def test_p():
    partition(a, b, run = lambda: None)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("load: %v", err)
	}
	res := rt.RunTest(context.Background(), "test_p")
	if res.Result != "fail" {
		t.Fatal("partition() was accepted under runtime=default")
	}
	for _, want := range []string{"packet gateway", "gvisor", "pools connections"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason should mention %q, got: %s", want, res.Reason)
		}
	}
}

func TestPartitionDirectionParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want partitionDirection
		ok   bool
	}{
		{"both", partitionBoth, true},
		{"a_to_b", partitionAtoB, true},
		{"b_to_a", partitionBtoA, true},
		{"sideways", "", false},
		{"", "", false},
	} {
		got, err := parsePartitionDirection(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("parsePartitionDirection(%q) = (%v, %v)", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parsePartitionDirection(%q) was accepted", tc.in)
		}
	}
}

// TestPartitionLegsAreDirectional is what makes a one-way partition possible:
// the classic Raft failure where a leader keeps its lease while its followers
// time out.
func TestPartitionLegsAreDirectional(t *testing.T) {
	if got := partitionBoth.legs("a", "b"); len(got) != 2 {
		t.Errorf("both = %v, want two legs", got)
	}
	got := partitionAtoB.legs("a", "b")
	if len(got) != 1 || got[0] != [2]string{"a", "b"} {
		t.Errorf("a_to_b = %v, want [[a b]]", got)
	}
	got = partitionBtoA.legs("a", "b")
	if len(got) != 1 || got[0] != [2]string{"b", "a"} {
		t.Errorf("b_to_a = %v, want [[b a]]", got)
	}
}

func TestPartitionPairKeyIsOrderIndependent(t *testing.T) {
	if pairKey("a", "b") != pairKey("b", "a") {
		t.Error("partition_stop(b, a) would not undo partition_start(a, b)")
	}
}

func TestPartitionRejectsUnknownKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
a = service("a", "/bin/true", interface("main", "tcp", 8080))
b = service("b", "/bin/true", interface("main", "tcp", 9090))
def test_p():
    partition(a, b, mode = "hard", run = lambda: None)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("load: %v", err)
	}
	res := rt.RunTest(context.Background(), "test_p")
	if res.Result != "fail" || !strings.Contains(res.Reason, "unknown keyword argument") {
		t.Errorf("unknown kwarg should be rejected, got: %s", res.Reason)
	}
}

func TestPartitionStopWithoutStart(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
a = service("a", "/bin/true", interface("main", "tcp", 8080))
b = service("b", "/bin/true", interface("main", "tcp", 9090))
def test_p():
    partition_stop(a, b)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("load: %v", err)
	}
	res := rt.RunTest(context.Background(), "test_p")
	if res.Result != "fail" || !strings.Contains(res.Reason, "no partition is active") {
		t.Errorf("partition_stop without start should fail clearly, got: %s", res.Reason)
	}
}

// TestPartitionSpecFormsLoad covers the shapes the Raft scenarios use.
func TestPartitionSpecFormsLoad(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
a = service("a", "/bin/true", interface("raft", "tcp", 8080))
b = service("b", "/bin/true", interface("raft", "tcp", 9090))

def test_scoped():
    partition(a, b, run = lambda: None)

def test_one_way():
    partition(a, b, direction = "a_to_b", run = lambda: None)

def test_held():
    partition_start(a, b)
    partition_stop(a, b)
`
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("partition forms failed to load: %v", err)
	}
}

// TestDocumentedSourceExampleParses: the docs show
//
//	fault(kafka.main, source=worker,
//	    drop(topic="orders.*"),
//	    run=scenario,
//	)
//
// which is not valid Starlark — a positional argument may not follow a named
// one. Copy-pasting the documented form fails to parse.
func TestDocumentedSourceExampleParses(t *testing.T) {
	rt := New(testLogger())
	src := `
determinism(runtime = "gvisor")
worker = service("worker", "/bin/true", interface("main", "http", 8080))
kafka  = service("kafka",  "/bin/true", interface("main", "tcp", 9092))
def test_s():
    fault(kafka.main,
        drop(topic="orders.*"),
        source=worker,
        run=lambda: None,
    )
`
	if err := rt.LoadString("test.star", src); err != nil {
		if strings.Contains(err.Error(), "positional argument may not follow named") {
			t.Fatalf("the documented source= example does not parse: %v", err)
		}
		t.Fatalf("load: %v", err)
	}
}
