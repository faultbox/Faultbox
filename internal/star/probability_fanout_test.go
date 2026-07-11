package star

import (
	"context"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/engine"
	"go.starlark.net/starlark"
)

// TestEnumerateLeaves_ProbabilityAxes — a single probability site
// with max_fires=2 fans out 4 leaves; each leaf's ProbabilityOutcomes
// vector is the binary encoding of its index.
func TestEnumerateLeaves_ProbabilityAxes(t *testing.T) {
	probAxes := []ProbFaultSite{{Key: "wal", MaxFires: 2, Prob: 0.3}}
	leaves := enumerateLeaves(nil, probAxes, nil)
	if len(leaves) != 4 {
		t.Fatalf("expected 4 leaves (2^max_fires), got %d", len(leaves))
	}
	// Leaf 0 should be all-false (occurrences 0 and 1 both not-fired
	// when digit=0 → bits 0,0). Leaf 3 (digit=3) → bits 1,1.
	if got := leaves[0].ProbabilityOutcomes["wal"]; got[0] || got[1] {
		t.Errorf("leaf 0 outcomes = %v, want [false, false]", got)
	}
	if got := leaves[3].ProbabilityOutcomes["wal"]; !got[0] || !got[1] {
		t.Errorf("leaf 3 outcomes = %v, want [true, true]", got)
	}
	// Leaf 1 → bit 0 set (occ 0 fires, occ 1 does not).
	if got := leaves[1].ProbabilityOutcomes["wal"]; !got[0] || got[1] {
		t.Errorf("leaf 1 outcomes = %v, want [true, false]", got)
	}
}

// TestEnumerateLeaves_ChoiceAndProbCrossProduct — combining one
// choice axis (cardinality 3) with one prob axis (cardinality 4)
// yields 12 leaves.
func TestEnumerateLeaves_ChoiceAndProbCrossProduct(t *testing.T) {
	axes := []*ChoiceVal{
		{Name: "k", Options: []starlark.Value{starlark.MakeInt(0), starlark.MakeInt(1), starlark.MakeInt(2)}},
	}
	prob := []ProbFaultSite{{Key: "wal", MaxFires: 2, Prob: 0.3}}
	leaves := enumerateLeaves(axes, prob, nil)
	if len(leaves) != 12 {
		t.Errorf("3 choices x 4 prob-leaves = 12, got %d", len(leaves))
	}
}

// TestProbabilityFanout_RecordsSiteOnFault — installing a fault
// with probability < 1, max_fires > 0, and exhaustive mode records a
// ProbFaultSite that the plan walker can enumerate. The recorded
// key is the rule's Label when set.
//
// This is a unit-level test on the recording path — full end-to-end
// (running 2^max_fires leaves with deterministic outcomes) requires a
// Linux seccomp environment and is left for the integration suite.
func TestProbabilityFanout_RecordsSiteOnFault(t *testing.T) {
	rt := New(testLogger())
	rt.recordProbFault(ProbFaultSite{Key: "wal", MaxFires: 3, Prob: 0.3})
	rt.recordProbFault(ProbFaultSite{Key: "wal", MaxFires: 3, Prob: 0.3}) // dedup
	rt.recordProbFault(ProbFaultSite{Key: "cache", MaxFires: 2, Prob: 0.5})
	sites := rt.bodyProbFaults()
	if len(sites) != 2 {
		t.Errorf("expected 2 sites (dedup by key), got %d: %+v", len(sites), sites)
	}
	rt.resetBodyProbFaults()
	if got := rt.bodyProbFaults(); len(got) != 0 {
		t.Errorf("reset should clear sites, got %d", len(got))
	}
}

// TestRunAll_MultiLeafBundleShape — running a spec with named
// choose() fan-out produces a SuiteResult whose TestResults carry
// LeafID. The CLI's testRowsFromResult mapping is exercised
// indirectly via this shape — see cmd/faultbox tests for the full
// manifest round-trip.
func TestRunAll_MultiLeafBundleShape(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_leaves():
    _ = choose("k", [10, 20])
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 2 {
		t.Fatalf("expected 2 leaf rows, got %d", len(res.Tests))
	}
	gotIDs := map[string]bool{res.Tests[0].LeafID: true, res.Tests[1].LeafID: true}
	if !gotIDs["0"] || !gotIDs["1"] {
		t.Errorf("expected LeafIDs {0,1}, got %v", gotIDs)
	}
	if res.Tests[0].Name != res.Tests[1].Name {
		t.Errorf("leaves should share Name, got %q vs %q", res.Tests[0].Name, res.Tests[1].Name)
	}
}

// TestProbabilityFanout_FamilyExpansionRecordsOneSite — review B3
// regression: an unlabeled write=deny(probability=0.3, max_fires=2)
// must record exactly one ProbFaultSite, not one per expanded
// syscall (write/writev/pwrite64). Otherwise the cross-product
// would multiply leaf count by 2^N for every sibling.
func TestProbabilityFanout_FamilyExpansionRecordsOneSite(t *testing.T) {
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox")
dn = deny("EIO", probability=0.3, max_fires=2)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	// Drive the recording path: apply the fault as the body would.
	// applyFaults skips engine work when there's no session, but the
	// recording branch fires before the engine bail-out.
	rt.faults = map[string]map[string]*FaultDef{"svc": {"write": rt.globals["dn"].(*FaultDef)}}
	rt.sessions = map[string]*runningSession{"svc": {session: nil}} // mock-style: no seccomp
	_ = rt.applyFaults("svc", rt.faults["svc"])
	sites := rt.bodyProbFaults()
	if len(sites) != 1 {
		t.Fatalf("expected 1 site for unlabeled write= fault (family expansion must dedup), got %d: %+v", len(sites), sites)
	}
	if sites[0].MaxFires != 2 {
		t.Errorf("MaxFires = %d, want 2", sites[0].MaxFires)
	}
}

// TestRunAll_LeafProbabilityOutcomesSurfaceOnTestResult — review
// B1 on PR #124: prove every leaf of a probability fan-out carries
// its actual fire/no-fire vector through to TestResult. The
// copyBoolVecMap deep-copy path was untested at the TestResult
// level before this regression guard.
func TestRunAll_LeafProbabilityOutcomesSurfaceOnTestResult(t *testing.T) {
	skipIfNoDocker(t)
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox")
dn = deny("EIO", probability=0.3, max_fires=2, mode="exhaustive", label="wal")
def test_prob():
    fault(svc, write=dn, run=lambda: None)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 4 {
		t.Fatalf("expected 4 leaves (2^max_fires), got %d", len(res.Tests))
	}
	seen := map[string]bool{}
	for _, tr := range res.Tests {
		if tr.LeafProbabilityOutcomes == nil {
			t.Errorf("leaf %s: LeafProbabilityOutcomes missing", tr.LeafID)
			continue
		}
		vec, ok := tr.LeafProbabilityOutcomes["wal"]
		if !ok {
			t.Errorf("leaf %s: missing wal key; have %v", tr.LeafID, tr.LeafProbabilityOutcomes)
			continue
		}
		if len(vec) != 2 {
			t.Errorf("leaf %s: vector len = %d, want 2", tr.LeafID, len(vec))
			continue
		}
		key := ""
		for _, b := range vec {
			if b {
				key += "1"
			} else {
				key += "0"
			}
		}
		seen[key] = true
	}
	for _, want := range []string{"00", "01", "10", "11"} {
		if !seen[want] {
			t.Errorf("missing vector %q in leaves; got %v", want, seen)
		}
	}
	// Single-leaf rows must omit the field entirely.
	rt2 := New(testLogger())
	_ = rt2.LoadString("spec.star", `def test_plain(): pass`)
	res2, _ := rt2.RunAll(context.Background(), RunConfig{})
	if res2.Tests[0].LeafProbabilityOutcomes != nil {
		t.Errorf("single-leaf result must omit LeafProbabilityOutcomes; got %v", res2.Tests[0].LeafProbabilityOutcomes)
	}
}

// TestRunAll_CombinedChooseAndProbCarryBothMaps — composite leaf
// (choose() × probability) must carry both maps. Guards against a
// future refactor that copies one kind but not the other.
func TestRunAll_CombinedChooseAndProbCarryBothMaps(t *testing.T) {
	skipIfNoDocker(t)
	rt := New(testLogger())
	src := `
svc = service("svc", image="busybox")
dn = deny("EIO", probability=0.3, max_fires=1, mode="exhaustive", label="wal")
def test_combo():
    _ = choose("retries", [0, 1])
    fault(svc, write=dn, run=lambda: None)
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	res, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(res.Tests) != 4 {
		t.Fatalf("expected 4 leaves (2 × 2), got %d", len(res.Tests))
	}
	for _, tr := range res.Tests {
		if _, ok := tr.LeafChoices["retries"]; !ok {
			t.Errorf("leaf %s missing retries in LeafChoices: %v", tr.LeafID, tr.LeafChoices)
		}
		if _, ok := tr.LeafProbabilityOutcomes["wal"]; !ok {
			t.Errorf("leaf %s missing wal in LeafProbabilityOutcomes: %v", tr.LeafID, tr.LeafProbabilityOutcomes)
		}
	}
}

// TestProbabilityDecider_ConsultsLeaf — the runtime's decider
// closure pulls the current leaf's outcomes vector and returns
// (fire, pinned) matching ProbabilityFire's contract. Falls back to
// (false, false) when the leaf is nil or the key is missing.
func TestProbabilityDecider_ConsultsLeaf(t *testing.T) {
	rt := New(testLogger())
	dec := rt.probabilityDecider("svc")

	// Nil leaf — unpinned.
	rule := &engine.FaultRule{Label: "wal", Syscall: "write"}
	if _, pinned := dec(rule, 0); pinned {
		t.Error("nil leaf must produce unpinned")
	}

	// Leaf pins the rule.
	rt.currentLeaf = &PlanLeaf{ProbabilityOutcomes: map[string][]bool{"wal": {true, false}}}
	if fire, pinned := dec(rule, 0); !fire || !pinned {
		t.Errorf("occurrence 0: got fire=%v pinned=%v, want true/true", fire, pinned)
	}
	if fire, pinned := dec(rule, 1); fire || !pinned {
		t.Errorf("occurrence 1: got fire=%v pinned=%v, want false/true", fire, pinned)
	}

	// Out-of-range falls back.
	if _, pinned := dec(rule, 5); pinned {
		t.Error("out-of-range occurrence must produce unpinned")
	}
}

// TestProbabilityFanout_MaxFiresParses — deny(..., probability=, max_fires=N)
// sets FaultDef.MaxFires.
func TestProbabilityFanout_MaxFiresParses(t *testing.T) {
	rt := New(testLogger())
	src := `dn = deny("EIO", probability=0.3, max_fires=4, label="wal")`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	dn, ok := rt.globals["dn"].(*FaultDef)
	if !ok {
		t.Fatalf("dn must be *FaultDef, got %T", rt.globals["dn"])
	}
	if dn.MaxFires != 4 {
		t.Errorf("MaxFires = %d, want 4", dn.MaxFires)
	}
	if dn.Probability != 0.3 {
		t.Errorf("Probability = %v, want 0.3", dn.Probability)
	}
}

// TestProbabilityFanout_ModeParses — mode="exhaustive" / "stochastic"
// flow through to FaultDef.Mode. RFC-043 Q2 (PR #125 N4): omitted
// mode= with max_fires>0 normalizes to "exhaustive" so the internal
// representation matches the documented default. Omitted mode= with
// no max_fires stays "" (no-fan-out / stochastic path).
func TestProbabilityFanout_ModeParses(t *testing.T) {
	cases := []struct {
		src  string
		want string
		note string
	}{
		{`d = deny("EIO", probability=0.5, mode="exhaustive", max_fires=3)`, "exhaustive", "explicit exhaustive"},
		{`d = deny("EIO", probability=0.5, mode="stochastic")`, "stochastic", "explicit stochastic"},
		{`d = deny("EIO", probability=0.5)`, "", "no fan-out: stays empty"},
		// Q2 normalization: max_fires implies exhaustive even when
		// mode= is omitted.
		{`d = deny("EIO", probability=0.5, max_fires=2)`, "exhaustive", "Q2 normalization"},
		{`d = deny("EIO", probability=0.3, max_fires=5)`, "exhaustive", "Q2 normalization, larger N"},
	}
	for _, tc := range cases {
		rt := New(testLogger())
		if err := rt.LoadString("spec.star", tc.src); err != nil {
			t.Fatalf("LoadString %q: %v", tc.src, err)
		}
		d := rt.globals["d"].(*FaultDef)
		if d.Mode != tc.want {
			t.Errorf("[%s] Mode for %q = %q, want %q", tc.note, tc.src, d.Mode, tc.want)
		}
	}
}

// TestProbabilityFanout_RejectsBadKwargs — validation errors fire at
// spec load, not at run time.
func TestProbabilityFanout_RejectsBadKwargs(t *testing.T) {
	cases := []struct {
		src      string
		wantSubs string
	}{
		{`d = deny("EIO", probability=0.5, max_fires="five")`, "max_fires must be an integer"},
		{`d = deny("EIO", probability=0.5, max_fires=0)`, "max_fires must be > 0"},
		{`d = deny("EIO", probability=0.5, max_fires=-1)`, "max_fires must be > 0"},
		{`d = deny("EIO", max_fires=3)`, "only meaningful with probability"},
		{`d = deny("EIO", probability=1.0, max_fires=3)`, "only meaningful with probability"},
		{`d = deny("EIO", probability=0.5, mode="random")`, "must be \"exhaustive\" or \"stochastic\""},
		{`d = deny("EIO", probability=0.5, max_fires=3, mode="stochastic")`, "incompatible with mode"},
		// Review B4: mode="exhaustive" without max_fires= would
		// silently degrade to stochastic at runtime. Reject at load.
		{`d = deny("EIO", probability=0.5, mode="exhaustive")`, "mode=\"exhaustive\" requires max_fires"},
	}
	for _, tc := range cases {
		rt := New(testLogger())
		err := rt.LoadString("spec.star", tc.src)
		if err == nil {
			t.Errorf("expected error for %q", tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubs) {
			t.Errorf("for %q, want error containing %q, got %v", tc.src, tc.wantSubs, err)
		}
	}
}

// TestProbabilityFanout_DelayAcceptsKwargs — same surface lands on
// delay() so the kwarg parser doesn't drift between builtins.
func TestProbabilityFanout_DelayAcceptsKwargs(t *testing.T) {
	rt := New(testLogger())
	src := `d = delay("500ms", probability=0.4, max_fires=2, mode="exhaustive")`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	d := rt.globals["d"].(*FaultDef)
	if d.MaxFires != 2 || d.Mode != "exhaustive" {
		t.Errorf("delay parsing wrong: MaxFires=%d, Mode=%q", d.MaxFires, d.Mode)
	}
}

// TestPlanLeaf_ProbabilityFire — leaf accessor returns (fire, pinned)
// from the per-rule outcomes vector and falls back to (_, false) on
// out-of-range or missing rule, matching the engine's "consult-or-RNG"
// contract.
func TestPlanLeaf_ProbabilityFire(t *testing.T) {
	leaf := &PlanLeaf{
		ProbabilityOutcomes: map[string][]bool{
			"wal": {true, false, true},
		},
	}
	if fire, pinned := leaf.ProbabilityFire("wal", 0); !fire || !pinned {
		t.Errorf("occurrence 0: got fire=%v pinned=%v, want true/true", fire, pinned)
	}
	if fire, pinned := leaf.ProbabilityFire("wal", 1); fire || !pinned {
		t.Errorf("occurrence 1: got fire=%v pinned=%v, want false/true", fire, pinned)
	}
	if _, pinned := leaf.ProbabilityFire("wal", 5); pinned {
		t.Error("out-of-range occurrence must report unpinned")
	}
	if _, pinned := leaf.ProbabilityFire("missing", 0); pinned {
		t.Error("missing rule key must report unpinned")
	}
	var nilLeaf *PlanLeaf
	if _, pinned := nilLeaf.ProbabilityFire("wal", 0); pinned {
		t.Error("nil leaf must report unpinned")
	}
}
