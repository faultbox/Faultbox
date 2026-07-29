package star

import (
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// A shaper on the default runtime must ERROR, not silently do nothing.
//
// Same reasoning as partition(): a bandwidth() that quietly no-ops leaves the
// test passing against a link that was never slow, and the run still looks
// like evidence. Refusing is the only outcome that cannot be misread.
func TestShapers_RefuseOnDefaultRuntime(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"bandwidth", `
def test_x():
    bandwidth(rate = "1mbit")
`},
		{"mtu", `
def test_x():
    mtu(size = 576)
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := New(testLogger())
			if err := rt.LoadString("spec.star", tc.spec); err != nil {
				t.Fatalf("LoadString: %v", err)
			}
			rt.inTest.Store(true)
			defer rt.inTest.Store(false)

			var err error
			if tc.name == "bandwidth" {
				_, err = rt.builtinBandwidth(nil, nil, nil, kwargsOf("rate", "1mbit"))
			} else {
				_, err = rt.builtinMTU(nil, nil, nil, kwargsOfInt("size", 576))
			}
			if err == nil {
				t.Fatalf("%s() on runtime=default must error, not no-op", tc.name)
			}
			for _, want := range []string{"packet gateway", "gvisor"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q; got %q", want, err.Error())
				}
			}
		})
	}
}

func TestBandwidth_RejectsBadArguments(t *testing.T) {
	rt := newGvisorRuntime(t)
	cases := []struct {
		name    string
		kwargs  []starlarkKV
		wantSub string
	}{
		{"unitless rate", []starlarkKV{{"rate", "1000000"}}, "no unit"},
		{"nonsense rate", []starlarkKV{{"rate", "quick"}}, "no unit"},
		{"zero rate", []starlarkKV{{"rate", "0mbit"}}, "must be positive"},
		{"bad direction", []starlarkKV{{"rate", "1mbit"}, {"dir", "up"}}, "dir must be"},
		{"bad queue", []starlarkKV{{"rate", "1mbit"}, {"queue", "soon"}}, "bad queue"},
		{"zero queue", []starlarkKV{{"rate", "1mbit"}, {"queue", "0s"}}, "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.builtinBandwidth(nil, nil, nil, kvsToKwargs(tc.kwargs))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error should contain %q; got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// An MTU below the IPv4 minimum cannot carry a fragmented datagram. Accepting
// it would model no real network while looking like a configured one.
func TestMTU_RejectsBelowIPv4Minimum(t *testing.T) {
	rt := newGvisorRuntime(t)
	for _, size := range []int{0, 1, 67, -100} {
		_, err := rt.builtinMTU(nil, nil, nil, kwargsOfInt("size", size))
		if err == nil {
			t.Errorf("mtu(size=%d) should be rejected", size)
			continue
		}
		if !strings.Contains(err.Error(), "IPv4 minimum") {
			t.Errorf("mtu(size=%d): error should explain the floor; got %q", size, err.Error())
		}
	}
}

// The rate is validated in the builtin, before the gateway is touched, so a
// typo is a spec error rather than a silently unshaped link.
func TestBandwidth_RateValidatedBeforeGatewayStart(t *testing.T) {
	rt := newGvisorRuntime(t)
	_, err := rt.builtinBandwidth(nil, nil, nil, kwargsOf("rate", "1zbit"))
	if err == nil {
		t.Fatal("an unknown unit must be rejected")
	}
	// If validation had been left to the gateway, the failure on a host with
	// no CAP_NET_ADMIN would be about the TUN device instead of the typo.
	if strings.Contains(err.Error(), "CAP_NET_ADMIN") || strings.Contains(err.Error(), "TUN") {
		t.Errorf("rate typo reported as a gateway problem: %q", err.Error())
	}
}

// clearShapers must be safe when nothing was installed and when no gateway
// ever started — it runs on every test teardown.
func TestClearShapers_SafeWhenUnused(t *testing.T) {
	rt := New(testLogger())
	rt.clearShapers() // never installed
	rt.shapersActive.Store(true)
	rt.clearShapers() // flagged, but no gateway exists
	if rt.shapersActive.Load() {
		t.Error("clearShapers must reset the flag even when there is no gateway")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

type starlarkKV struct{ k, v string }

func newGvisorRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt := New(testLogger())
	if err := rt.LoadString("spec.star", "determinism(runtime = \"gvisor\")\n"); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	rt.inTest.Store(true)
	t.Cleanup(func() { rt.inTest.Store(false) })
	return rt
}

func kwargsOf(k, v string) []starlark.Tuple {
	return []starlark.Tuple{{starlark.String(k), starlark.String(v)}}
}

func kwargsOfInt(k string, v int) []starlark.Tuple {
	return []starlark.Tuple{{starlark.String(k), starlark.MakeInt(v)}}
}

func kvsToKwargs(kvs []starlarkKV) []starlark.Tuple {
	out := make([]starlark.Tuple, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, starlark.Tuple{starlark.String(kv.k), starlark.String(kv.v)})
	}
	return out
}
