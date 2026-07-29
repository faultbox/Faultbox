package star

import (
	"fmt"
	"time"

	"github.com/faultbox/Faultbox/internal/netfault"
	"go.starlark.net/starlark"
)

// bandwidth() and mtu() — link-scoped shapers (RFC-054, deferred from v0.14.0).
//
// Unlike every packet_* builtin these take no matcher, because they do not
// describe packets. A rule says "what happens to packets that look like this";
// a shaper says "what kind of link is this". The distinction is not cosmetic:
// approximating mtu() as packet_drop(len_gt=N) — which is what v0.14.0's
// scenario 8 had to do — drops oversized packets, which looks like a black
// hole and behaves like nothing real. An actual small-MTU path makes TCP
// negotiate a smaller MSS and makes IP fragment, and that behaviour is where
// the interesting bugs are.
//
// Both are gateway-wide. There is one TUN link under one FaultEndpoint, so
// per-interface capacity is not something the gateway has to give; offering a
// per-target knob would be offering one that lies.

// builtinBandwidth implements bandwidth(rate=, dir="both", queue="250ms").
func (rt *Runtime) builtinBandwidth(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var rate, dirStr, queueStr string
	dirStr = "both"
	if err := starlark.UnpackArgs("bandwidth", args, kwargs,
		"rate", &rate,
		"dir?", &dirStr,
		"queue?", &queueStr,
	); err != nil {
		return nil, err
	}
	if err := rt.requireShaperRuntime("bandwidth"); err != nil {
		return nil, err
	}
	dir, err := parseShaperDir("bandwidth", dirStr)
	if err != nil {
		return nil, err
	}
	// Validate the rate here rather than inside the gateway, so a typo is a
	// spec error naming the accepted forms instead of a silently unshaped link.
	bps, err := netfault.ParseRate(rate)
	if err != nil {
		return nil, fmt.Errorf("bandwidth(): %w", err)
	}
	backlog := netfault.DefaultShaperBacklog
	if queueStr != "" {
		d, err := parseStarDuration(queueStr)
		if err != nil {
			return nil, fmt.Errorf("bandwidth() bad queue %q: %w", queueStr, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("bandwidth() queue must be positive, got %q", queueStr)
		}
		backlog = d
	}

	gw, err := rt.ensurePacketGateway()
	if err != nil {
		return nil, fmt.Errorf("bandwidth(): %w", err)
	}
	if gw == nil {
		return nil, fmt.Errorf("bandwidth(): no packet gateway is active")
	}
	if err := gw.SetBandwidth(rate, dir, backlog); err != nil {
		return nil, err
	}
	rt.shapersActive.Store(true)
	rt.events.Emit("bandwidth_applied", "", map[string]string{
		"rate":        rate,
		"bytes_per_s": fmt.Sprintf("%.0f", bps),
		"dir":         dirStr,
		"queue":       backlog.String(),
	})
	return starlark.None, nil
}

// builtinMTU implements mtu(size=).
func (rt *Runtime) builtinMTU(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var size int
	if err := starlark.UnpackArgs("mtu", args, kwargs, "size", &size); err != nil {
		return nil, err
	}
	if err := rt.requireShaperRuntime("mtu"); err != nil {
		return nil, err
	}
	// 68 is the IPv4 minimum every host must handle (RFC 791). Below it the
	// stack cannot fragment a datagram at all, so the link would not be "small
	// MTU", it would be broken in a way no real network is.
	const minIPv4MTU = 68
	if size < minIPv4MTU {
		return nil, fmt.Errorf(
			"mtu(size=%d) is below the IPv4 minimum of %d; a path that small cannot "+
				"carry a fragmented datagram and does not model any real network",
			size, minIPv4MTU)
	}

	gw, err := rt.ensurePacketGateway()
	if err != nil {
		return nil, fmt.Errorf("mtu(): %w", err)
	}
	if gw == nil {
		return nil, fmt.Errorf("mtu(): no packet gateway is active")
	}
	if err := gw.SetMTU(uint32(size)); err != nil {
		return nil, err
	}
	rt.shapersActive.Store(true)
	rt.events.Emit("mtu_applied", "", map[string]string{"size": fmt.Sprintf("%d", size)})
	return starlark.None, nil
}

func parseShaperDir(builtin, s string) (netfault.Direction, error) {
	switch s {
	case "both", "":
		return netfault.DirBoth, nil
	case "c2s":
		return netfault.DirC2S, nil
	case "s2c":
		return netfault.DirS2C, nil
	}
	return 0, fmt.Errorf("%s() dir must be \"c2s\", \"s2c\" or \"both\", got %q", builtin, s)
}

// requireShaperRuntime refuses rather than silently doing nothing.
//
// Same reasoning as partition(): a shaper that quietly no-ops leaves the test
// passing against a link that was never slow, which is worse than an error
// because the run still looks like evidence.
func (rt *Runtime) requireShaperRuntime(builtin string) error {
	rt.mu.Lock()
	runtimeName := rt.detRuntime
	rt.mu.Unlock()
	if runtimeSupportsPacketFaults(runtimeName) {
		return nil
	}
	return fmt.Errorf(
		"%s() needs the packet gateway, but this spec runs on runtime=%q; "+
			"add determinism(runtime=%q) at the top of the spec",
		builtin, runtimeName, DeterminismRuntimeGVisor)
}

// clearShapers removes link shapers at test end so one test's slow link cannot
// silently become the next test's baseline.
func (rt *Runtime) clearShapers() {
	if !rt.shapersActive.Swap(false) {
		return
	}
	gw := rt.packetGatewayHandle()
	if gw == nil {
		return
	}
	// Report what the shaper actually did before dropping it. "The link was
	// configured slow" and "the link was the bottleneck" are different claims,
	// and only the second is evidence.
	for _, d := range []netfault.Direction{netfault.DirC2S, netfault.DirS2C} {
		st, ok := gw.ShaperStats(d)
		if !ok {
			continue
		}
		rt.events.Emit("bandwidth_stats", "", map[string]string{
			"dir":          d.String(),
			"admitted":     fmt.Sprintf("%d", st.Admitted),
			"dropped":      fmt.Sprintf("%d", st.Dropped),
			"peak_backlog": st.PeakBacklog.Round(time.Millisecond).String(),
		})
	}
	gw.ClearShapers()
}
