package star

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/faultbox/Faultbox/internal/gvisor"
	"github.com/faultbox/Faultbox/internal/gvisor/seccheck"
)

// Filesystem-observation lifecycle (RFC-054 M5).
//
// One sink per spec, one trace session per container. The sink must be
// listening before the sandbox starts: the trace config sets
// ignore_setup_error=false, so a Sentry that cannot reach the sink refuses to
// proceed — which is what we want, since a silently absent sink would leave
// watch() observing nothing.

type fsObservation struct {
	mu   sync.Mutex
	sink *seccheck.Sink
	// containers maps a sandbox's container ID to the service it runs, so a
	// trace point can be attributed. The session itself is installed at boot
	// by the runtime flag, so there is nothing per-container to start or stop.
	containers map[string]string
	sessions   map[string]*gvisor.TraceSession // service → session (unused; see attachTrace)
	started    bool
	// connected records whether any sandbox ever completed the handshake.
	// A watch() window that ran while this is false observed nothing, and
	// must not be reported as a pass.
	connected bool
	decodeErr error
	// unattributed counts points whose container ID matched no launched
	// service. Non-zero means observation is running but the trace is being
	// discarded — which looks exactly like "the SUT did no I/O".
	unattributed int
}

func newFSObservation() *fsObservation {
	return &fsObservation{
		containers: make(map[string]string),
		sessions:   make(map[string]*gvisor.TraceSession),
	}
}

// fileObservationEnabled reports whether the spec asked for it. Driven by
// watch() usage rather than by the runtime alone: a spec on runtime="gvisor"
// that never calls watch() should not pay for runsc.
func (rt *Runtime) fileObservationEnabled() bool {
	rt.mu.Lock()
	runtimeName := rt.detRuntime
	rt.mu.Unlock()
	if runtimeName != DeterminismRuntimeGVisor {
		return false
	}
	return rt.specUsesWatch
}

// containerRuntimeName returns the OCI runtime for container launches, empty
// for the daemon default.
func (rt *Runtime) containerRuntimeName() string {
	if rt.fileObservationEnabled() {
		// The registered trace runtime, not bare runsc: the trace session is
		// installed at sandbox boot via --pod-init-config, and only this
		// runtime carries that flag. Plain runsc would start the container
		// perfectly well and observe nothing.
		return gvisor.TraceRuntimeName
	}
	return ""
}

// ensureFSObservation starts the sink once, before the first container.
func (rt *Runtime) ensureFSObservation(ctx context.Context) error {
	if !rt.fileObservationEnabled() {
		// Say why, once. "My watch() sees nothing" is otherwise an invisible
		// failure: the spec loads, the services start, and the trace is simply
		// empty.
		rt.mu.Lock()
		runtimeName := rt.detRuntime
		usesWatch := rt.specUsesWatch
		skipped := rt.fsSkipReported
		rt.fsSkipReported = true
		rt.mu.Unlock()
		if !skipped && usesWatch {
			rt.events.Emit("fs_observation_skipped", "", map[string]string{
				"runtime": runtimeName,
				"reason": fmt.Sprintf("spec calls watch() but runtime=%q; filesystem observation needs runtime=%q",
					runtimeName, DeterminismRuntimeGVisor),
			})
		}
		return nil
	}
	st := rt.fsObs
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.started {
		return nil
	}

	avail := gvisor.CheckAvailability(ctx)
	if !avail.OK() {
		return avail.Err
	}

	// The endpoint is not ours to choose. Sandboxes learn where to report from
	// the host trace config, written once by `faultbox setup-trace`; binding
	// anywhere else would produce a sink nothing ever connects to, and every
	// watch() assertion would pass having observed nothing.
	sink, err := gvisor.AcquireSink(
		"",
		func(io seccheck.FileIO) {
			st.mu.Lock()
			st.connected = true
			st.mu.Unlock()
			rt.routeFileIO(io)
		},
		func(e error) {
			st.mu.Lock()
			if st.decodeErr == nil {
				st.decodeErr = e
			}
			st.mu.Unlock()
		},
	)
	if err != nil {
		return fmt.Errorf("start filesystem observation: %w", err)
	}
	st.sink = sink
	st.started = true

	rt.events.Emit("fs_observation_started", "", map[string]string{
		"socket":       sink.Path(),
		"runsc":        avail.Version,
		"runsc_binary": avail.BinaryPath,
	})
	return nil
}

// attachTrace records that a launched container is under observation.
//
// It no longer STARTS anything. v0.14.0 called `runsc trace create` here,
// which is why watch() was withdrawn: that command instruments only tasks
// created after the session begins, and Faultbox attaches once a service is
// healthy — by which point every worker thread already exists. Measured, a
// network-driven workload produced 2 trace points where the same SQL from a
// freshly spawned process produced 1054.
//
// The session now comes from --pod-init-config, installed at sandbox boot by
// the faultbox-trace runtime, so every task is instrumented from its first
// instruction. Measured on the same workload: 236 points, and 11,295 before
// any query at all. See docs/design/2026-07-29-pod-init-config-spike.md.
//
// What remains is bookkeeping: the container must be recorded so points
// carrying its ID can be attributed to a service rather than counted as
// unattributed.
func (rt *Runtime) attachTrace(ctx context.Context, service, containerID string) error {
	if !rt.fileObservationEnabled() {
		return nil
	}
	st := rt.fsObs
	st.mu.Lock()
	if st.sink == nil {
		st.mu.Unlock()
		return fmt.Errorf("filesystem observation: sink is not running")
	}
	st.containers[containerID] = service
	st.mu.Unlock()

	rt.events.Emit("fs_trace_attached", service, map[string]string{"container": containerID})
	return nil
}

// detachTrace forgets a container. There is no session to stop — the sandbox
// carried its own, and it dies with the sandbox.
func (rt *Runtime) detachTrace(_ context.Context, service string) {
	st := rt.fsObs
	st.mu.Lock()
	for id, svc := range st.containers {
		if svc == service {
			delete(st.containers, id)
		}
	}
	st.mu.Unlock()
}

// closeFSObservation tears everything down at session end.
func (rt *Runtime) closeFSObservation(ctx context.Context) {
	st := rt.fsObs
	st.mu.Lock()
	sink := st.sink
	sessions := st.sessions
	st.sink, st.sessions, st.started = nil, make(map[string]*gvisor.TraceSession), false
	st.mu.Unlock()

	for svc, sess := range sessions {
		if err := sess.Stop(ctx); err != nil {
			rt.log.Warn("stop trace session", "service", svc, "error", err.Error())
		}
	}
	if sink == nil {
		return
	}
	if dropped := sink.Dropped(); dropped > 0 {
		// Incomplete observation must be visible: a watch that missed points
		// can assert "never wrote outside /data" having simply not seen it.
		rt.events.Emit("fs_points_dropped", "", map[string]string{
			"count":  fmt.Sprintf("%d", dropped),
			"detail": "the Sentry dropped trace points; filesystem observation for this run is incomplete",
		})
	}
	st.mu.Lock()
	unattributed := st.unattributed
	st.mu.Unlock()
	if unattributed > 0 {
		rt.events.Emit("fs_points_unattributed", "", map[string]string{
			"count":  fmt.Sprintf("%d", unattributed),
			"detail": "trace points arrived but matched no launched container; observation was discarded",
		})
	}
	rt.events.Emit("fs_observation_stopped", "", map[string]string{
		"points":       fmt.Sprintf("%d", sink.Points()),
		"unattributed": fmt.Sprintf("%d", unattributed),
	})
	_ = sink.Close()
}

// routeFileIO attributes an operation to a service and hands it to watch().
//
// The Sentry reports a container ID, so attribution goes through the launched
// container map rather than guessing.
func (rt *Runtime) routeFileIO(io seccheck.FileIO) {
	service := rt.serviceForContainer(io.ContainerID)
	if service == "" {
		// Fall back to the sole traced service when there is exactly one.
		// The Sentry's container ID format is not guaranteed to match
		// Docker's, and dropping every point on a mismatch is
		// indistinguishable from "the SUT did no I/O" — the failure mode this
		// release exists to eliminate. With one sandbox there is no ambiguity
		// about who did the work.
		service = rt.soleTracedService()
	}
	if service == "" {
		rt.fsObs.mu.Lock()
		rt.fsObs.unattributed++
		rt.fsObs.mu.Unlock()
		return
	}
	rt.onFileIO(service, io)
}

// soleTracedService returns the only observed service, or "".
//
// Reads st.containers, which is what records an observed sandbox since the
// trace session moved to sandbox boot. It read st.sessions until M3 stopped
// populating that map — leaving the fallback silently dead, so every point
// whose container ID did not match exactly would have been counted as
// unattributed and thrown away. Precisely the "looks like the SUT did no I/O"
// failure this fallback exists to prevent.
func (rt *Runtime) soleTracedService() string {
	st := rt.fsObs
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.containers) != 1 {
		return ""
	}
	for _, name := range st.containers {
		return name
	}
	return ""
}

// serviceForContainer maps a Sentry container ID back to a service name.
func (rt *Runtime) serviceForContainer(containerID string) string {
	if containerID == "" {
		return ""
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for name, cid := range rt.containerIDs {
		// The Sentry may report a short ID.
		if cid == containerID || (len(cid) >= len(containerID) && cid[:len(containerID)] == containerID) {
			return name
		}
	}
	return ""
}

// fsObservationFailure reports why a watch() window cannot be trusted, or "".
//
// A watch that ran while nothing was ever observed is the filesystem analogue
// of an unwired packet gateway: the assertions below it are vacuous, and the
// test would pass having verified nothing.
func (rt *Runtime) fsObservationFailure() string {
	if !rt.fileObservationEnabled() {
		return ""
	}
	st := rt.fsObs
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.started {
		return "watch() ran but filesystem observation never started, so no I/O was seen"
	}
	if !st.connected {
		return "watch() ran but no sandbox ever connected to the trace sink, so no I/O was seen. " +
			"The services must be container-mode and launched under the " + gvisor.TraceRuntimeName +
			" runtime, which carries the boot-time trace session; a container under plain runsc " +
			"starts normally and reports nothing. Check: faultbox setup-trace --check"
	}
	if st.decodeErr != nil {
		return fmt.Sprintf("filesystem observation hit a decode error, so the trace is incomplete: %v", st.decodeErr)
	}
	// Dropped points make the observation a SUBSET of what happened, and the
	// canonical watch() assertion is a negative one — "this service never
	// wrote outside its data directory". A dropped point could be the
	// violating one, so the audit can no longer claim "never", only "never,
	// among those I saw". That is not the assertion the author wrote.
	//
	// Measured: the sink starts losing points between roughly 17k and 47k per
	// second, and enabling read tracing took a read-heavy workload from zero
	// drops to 1,488. Hence read being opt-in — see RFC-056 §0c.
	if st.sink != nil {
		if reason := droppedFailure(st.sink.Dropped()); reason != "" {
			return reason
		}
	}
	// Points arrived but belonged to no service we launched. Observation is
	// running and the trace is being discarded, which looks identical to "the
	// SUT did no I/O".
	if st.unattributed > 0 {
		return fmt.Sprintf(
			"filesystem observation received %d trace point(s) that matched no launched "+
				"service, so they were discarded. The watch below saw less than the run "+
				"produced; this usually means a container ID the Sentry reports differs from "+
				"the one Faultbox recorded", st.unattributed)
	}
	return ""
}

// droppedFailure reports why dropped trace points invalidate a watch, or "".
//
// Split from the guard so it is testable on any platform: reading a real drop
// count needs a live SOCK_SEQPACKET sink, which is Linux-only, and the M0b
// finding this encodes is too important to be exercised on one OS.
//
// Drops make the observation a SUBSET of what happened, and the canonical
// watch() assertion is negative — "this service never wrote outside its data
// directory". A dropped point could be the violating one, so the audit can no
// longer claim "never", only "never, among those I saw". That is not the
// assertion the author wrote, and the difference is invisible in the result.
//
// Measured: the sink starts losing points between roughly 17k and 47k per
// second, and enabling read tracing took a read-heavy workload from zero drops
// to 1,488. That measurement is why read is opt-in — see RFC-056 §0c.
func droppedFailure(n int64) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"filesystem observation dropped %d trace point(s), so what was observed is a "+
			"subset of what happened and a watch() assertion cannot be trusted — a "+
			"dropped operation could be the one it was looking for. This is a volume "+
			"limit rather than a mistake in the spec: narrow files= or ops=, or if this "+
			"host enables read tracing, turn it off (it roughly doubles traffic)", n)
}

// sourceUsesWatch reports whether a spec calls watch() or watch_start().
//
// A static scan rather than a runtime signal: the container runtime has to be
// chosen before the first service launches, which is long before any test body
// executes. The same approach validateMonitorLambdasInSource already uses.
//
// Over-detection is harmless in the wrong direction only — a false positive
// costs an unnecessary runsc requirement, which surfaces as a clear spec-load
// error, whereas a false negative would silently launch under runc and leave
// watch() observing nothing.
func sourceUsesWatch(src string) bool {
	for _, call := range []string{"watch(", "watch_start("} {
		idx := 0
		for {
			i := indexFrom(src, call, idx)
			if i < 0 {
				break
			}
			// Skip identifiers that merely end in "watch", e.g. "unwatch(".
			if i == 0 || !isIdentByte(src[i-1]) {
				return true
			}
			idx = i + 1
		}
	}
	return false
}

func indexFrom(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	i := strings.Index(s[from:], sub)
	if i < 0 {
		return -1
	}
	return from + i
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
