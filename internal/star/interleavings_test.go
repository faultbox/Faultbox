package star

import (
	"context"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// TestInterleavings_Parsing covers the int + string + omitted cases
// for the RFC-042 §8.8 interleavings= kwarg. Validation is centralized
// in parseInterleavingsKwarg so it stays consistent if/when wait_all,
// wait_n, wait_first land alongside parallel() in a later slice.
func TestInterleavings_Parsing(t *testing.T) {
	mkKwargs := func(v starlark.Value) []starlark.Tuple {
		return []starlark.Tuple{{starlark.String("interleavings"), v}}
	}
	cases := []struct {
		name    string
		kwargs  []starlark.Tuple
		wantKnd string
		wantN   int
	}{
		{"omitted defaults to single", nil, "single", 0},
		{"int 1 collapses to single", mkKwargs(starlark.MakeInt(1)), "single", 0},
		{"int N stored as n-cap", mkKwargs(starlark.MakeInt(4)), "n", 4},
		{"string \"1\" collapses to single", mkKwargs(starlark.String("1")), "single", 0},
		{"string \"all\"", mkKwargs(starlark.String("all")), "all", 0},
		{"string \"critical\"", mkKwargs(starlark.String("critical")), "critical", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseInterleavingsKwarg("parallel", tc.kwargs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Kind != tc.wantKnd || p.N != tc.wantN {
				t.Errorf("got %+v, want Kind=%q N=%d", p, tc.wantKnd, tc.wantN)
			}
		})
	}
}

// TestInterleavings_Rejections — reserved values and bad inputs must
// surface explicit messages so CI integrations gating on the kwarg
// don't drift when RFC-009/DPOR lands.
func TestInterleavings_Rejections(t *testing.T) {
	mkKwargs := func(v starlark.Value) []starlark.Tuple {
		return []starlark.Tuple{{starlark.String("interleavings"), v}}
	}
	cases := []struct {
		name     string
		kwargs   []starlark.Tuple
		wantSubs string
	}{
		{"int 0 rejected", mkKwargs(starlark.MakeInt(0)), "positive integer"},
		{"int negative rejected", mkKwargs(starlark.MakeInt(-3)), "positive integer"},
		{"string unknown rejected", mkKwargs(starlark.String("random")), "must be 1, an integer N"},
		{"dpor reserved with RFC ref", mkKwargs(starlark.String("dpor")), "RFC-009"},
		{"sut-internal reserved with L5 ref", mkKwargs(starlark.String("sut-internal")), "L5"},
		{"non-int non-string rejected", mkKwargs(starlark.True), "must be int or string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseInterleavingsKwarg("parallel", tc.kwargs)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestInterleavings_PolicyString — stable string form for plan.json
// and bundle manifest consumers.
func TestInterleavings_PolicyString(t *testing.T) {
	cases := []struct {
		p    InterleavingPolicy
		want string
	}{
		{InterleavingPolicy{Kind: "single"}, "1"},
		{InterleavingPolicy{Kind: "all"}, "all"},
		{InterleavingPolicy{Kind: "critical"}, "critical"},
		{InterleavingPolicy{Kind: "n", N: 5}, "5"},
	}
	for _, tc := range cases {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("policy %+v: String()=%q, want %q", tc.p, got, tc.want)
		}
	}
}

// TestInterleavings_ParallelAcceptsKwarg — wiring sanity. The kwarg
// is accepted at the parallel() call surface today; the plan walker
// that consumes it lands in PR 3 of this slice. For now we just
// verify spec load succeeds with the new kwarg present.
func TestInterleavings_ParallelAcceptsKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def body():
    parallel(a, b, interleavings="all")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
}

// TestInterleavings_ParallelRejectsBadKwarg — bad value surfaces at
// runtime (parallel() is body-time, not load-time).
func TestInterleavings_ParallelRejectsBadKwarg(t *testing.T) {
	rt := New(testLogger())
	src := `
def a(): pass
def b(): pass
def test_bad():
    parallel(a, b, interleavings="dpor")
`
	if err := rt.LoadString("spec.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	tr := rt.RunTest(context.Background(), "test_bad")
	if tr.Result != "fail" {
		t.Fatalf("Result = %q, want fail; reason: %s", tr.Result, tr.Reason)
	}
	if !strings.Contains(tr.Reason, "RFC-009") {
		t.Errorf("Reason should cite RFC-009; got %q", tr.Reason)
	}
}
