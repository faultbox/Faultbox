package star

import (
	"reflect"
	"testing"
)

func TestChooseAxes_LiteralLists(t *testing.T) {
	src := `
def test_timing():
    warmup = choose("warmup", [0, 10, 40])
    gap    = choose("gap", ["0ms", "400ms", "1200ms"])
    hold   = choose("hold", ["100ms", "900ms"])
`
	axes := chooseAxesInSource("spec.star", src)["test_timing"]
	if len(axes) != 3 {
		t.Fatalf("got %d axes, want 3: %v", len(axes), sortedAxisNames(axes))
	}
	// Source order, not sorted: enumerateLeaves fans out in axis order, so a
	// reordered plan tree would print a different leaf sequence than the run.
	if got := []string{axes[0].Name, axes[1].Name, axes[2].Name}; !reflect.DeepEqual(
		got, []string{"warmup", "gap", "hold"}) {
		t.Errorf("axis order = %v, want [warmup gap hold]", got)
	}
	if n := ChooseLeafCount(axes); n != 18 {
		t.Errorf("leaf count = %d, want 18 (3*3*2)", n)
	}
	if !ChooseAxesComplete(axes) {
		t.Error("all option lists are literals; the result should not be an estimate")
	}
	if got := axes[1].Values; !reflect.DeepEqual(got, []string{"0ms", "400ms", "1200ms"}) {
		t.Errorf("gap values = %v", got)
	}
}

// The count is what a cost gate depends on, so a computed list must widen the
// estimate rather than silently contribute a factor of 1 that looks exact.
func TestChooseAxes_ComputedListIsEstimated(t *testing.T) {
	src := `
OPTS = [1, 2, 3]
def test_dyn():
    a = choose("dyn", OPTS)
    b = choose("lit", ["x", "y"])
`
	axes := chooseAxesInSource("spec.star", src)["test_dyn"]
	if len(axes) != 2 {
		t.Fatalf("got %d axes, want 2", len(axes))
	}
	if axes[0].Known {
		t.Error("an axis over a variable must not claim a known option list")
	}
	if !axes[1].Known {
		t.Error("a literal list must be readable")
	}
	if ChooseAxesComplete(axes) {
		t.Error("a spec with one unreadable axis must report as estimated")
	}
	// Floor of 1 for the unknown axis: 1 * 2.
	if n := ChooseLeafCount(axes); n != 2 {
		t.Errorf("leaf count = %d, want 2 (lower bound)", n)
	}
}

// Anonymous choose([...]) still multiplies the leaf count even though assume()
// cannot name it. Dropping it would under-report the cost.
func TestChooseAxes_AnonymousStillCounts(t *testing.T) {
	src := `
def test_anon():
    a = choose([1, 2, 3])
    b = choose([True, False])
`
	axes := chooseAxesInSource("spec.star", src)["test_anon"]
	if len(axes) != 2 {
		t.Fatalf("got %d axes, want 2", len(axes))
	}
	if n := ChooseLeafCount(axes); n != 6 {
		t.Errorf("leaf count = %d, want 6", n)
	}
	for _, a := range axes {
		if a.Name == "" {
			t.Error("anonymous axes still need a display label")
		}
	}
}

func TestChooseAxes_NoChooseCalls(t *testing.T) {
	src := `
def test_plain():
    assert_true(True, "")
`
	if got := chooseAxesInSource("spec.star", src); len(got) != 0 {
		t.Errorf("a spec without choose() should yield no axes, got %v", got)
	}
	if n := ChooseLeafCount(nil); n != 1 {
		t.Errorf("no axes must mean one leaf, got %d", n)
	}
}

// A choose() inside a helper is attributed to no test: static analysis cannot
// resolve the call graph without executing it. Under-reporting here is
// deliberate — the alternative is inventing axes.
func TestChooseAxes_HelperCallsNotAttributed(t *testing.T) {
	src := `
def helper():
    return choose("hidden", [1, 2])

def test_uses_helper():
    x = helper()
`
	axes := chooseAxesInSource("spec.star", src)
	if len(axes["test_uses_helper"]) != 0 {
		t.Errorf("choose() in a helper must not be attributed to the caller, got %v",
			axes["test_uses_helper"])
	}
	if len(axes["helper"]) != 1 {
		t.Errorf("the helper itself should still record its axis, got %v", axes["helper"])
	}
}

// A spec that does not parse must not panic or invent axes — the canonical
// syntax error comes from ExecFile.
func TestChooseAxes_UnparseableSourceIsSilent(t *testing.T) {
	if got := chooseAxesInSource("spec.star", "def test_x(:\n"); got != nil {
		t.Errorf("unparseable source should yield nil, got %v", got)
	}
	if got := chooseAxesInSource("spec.star", ""); got != nil {
		t.Errorf("empty source should yield nil, got %v", got)
	}
}

// End-to-end through the runtime, which is how plan.Enumerate reaches it.
func TestChooseAxesByTest_ThroughRuntime(t *testing.T) {
	rt := New(testLogger())
	src := `
def test_a():
    x = choose("k", ["p", "q"])

def test_b():
    assert_true(True, "")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	byTest := rt.ChooseAxesByTest()
	if n := ChooseLeafCount(byTest["test_a"]); n != 2 {
		t.Errorf("test_a leaf count = %d, want 2", n)
	}
	if n := ChooseLeafCount(byTest["test_b"]); n != 1 {
		t.Errorf("test_b leaf count = %d, want 1", n)
	}
}
