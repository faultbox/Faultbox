package star

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	mu       sync.Mutex
	sink     *seccheck.Sink
	sessions map[string]*gvisor.TraceSession // service → session
	started  bool
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
	return &fsObservation{sessions: make(map[string]*gvisor.TraceSession)}
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
		return gvisor.RuntimeName
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

	// Keep the socket path short: a Unix socket path is capped near 104 bytes
	// and a long temp dir silently turns into "invalid argument".
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("fb-seccheck-%d.sock", os.Getpid()))
	sink, err := seccheck.Listen(seccheck.Config{
		Path: sockPath,
		OnFileIO: func(io seccheck.FileIO) {
			st.mu.Lock()
			st.connected = true
			st.mu.Unlock()
			rt.routeFileIO(io)
		},
		OnError: func(e error) {
			st.mu.Lock()
			if st.decodeErr == nil {
				st.decodeErr = e
			}
			st.mu.Unlock()
		},
	})
	if err != nil {
		return fmt.Errorf("start filesystem observation: %w", err)
	}
	st.sink = sink
	st.started = true

	rt.events.Emit("fs_observation_started", "", map[string]string{
		"socket":       sockPath,
		"runsc":        avail.Version,
		"runsc_binary": avail.BinaryPath,
	})
	return nil
}

// attachTrace starts a trace session on a launched container.
func (rt *Runtime) attachTrace(ctx context.Context, service, containerID string) error {
	if !rt.fileObservationEnabled() {
		return nil
	}
	st := rt.fsObs
	st.mu.Lock()
	sink := st.sink
	st.mu.Unlock()
	if sink == nil {
		return fmt.Errorf("filesystem observation: sink is not running")
	}

	sess, err := gvisor.StartTrace(ctx, containerID, sink.Path(), gvisor.FileIOPoints())
	if err != nil {
		return fmt.Errorf("attach trace to %s: %w", service, err)
	}
	st.mu.Lock()
	st.sessions[service] = sess
	st.mu.Unlock()

	rt.events.Emit("fs_trace_attached", service, map[string]string{"container": containerID})
	return nil
}

// detachTrace stops the trace session for one service.
func (rt *Runtime) detachTrace(ctx context.Context, service string) {
	st := rt.fsObs
	st.mu.Lock()
	sess := st.sessions[service]
	delete(st.sessions, service)
	st.mu.Unlock()
	if sess == nil {
		return
	}
	if err := sess.Stop(ctx); err != nil {
		rt.log.Warn("stop trace session", "service", service, "error", err.Error())
	}
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

// soleTracedService returns the only service with a trace session, or "".
func (rt *Runtime) soleTracedService() string {
	st := rt.fsObs
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.sessions) != 1 {
		return ""
	}
	for name := range st.sessions {
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
		return "watch() ran but no sandbox ever connected to the trace sink, so no I/O was seen; " +
			"check that the services are container-mode and running under runsc"
	}
	if st.decodeErr != nil {
		return fmt.Sprintf("filesystem observation hit a decode error, so the trace is incomplete: %v", st.decodeErr)
	}
	return ""
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
