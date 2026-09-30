package star

import (
	"context"
	"fmt"
	"time"

	"github.com/faultbox/Faultbox/internal/engine"
)

// Each operation is bounded separately and attributed to a concrete service.
// A failed shutdown poisons this runtime: no later test may reuse its resources.
func (rt *Runtime) shutdownPhase(service, phase string, fn func(context.Context) error) bool {
	timeout := rt.shutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("panic during shutdown: %v", recovered)
			}
		}()
		done <- fn(ctx)
	}()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil {
		return true
	}
	rt.shutdownFailed.Store(true)
	rt.log.Error("shutdown failed", "service", service, "phase", phase, "timeout", timeout, "error", err.Error())
	rt.events.recordOnly("service_stop_error", service, map[string]string{"code": "TEARDOWN_FAILED", "phase": phase, "error": err.Error(), "timeout_ms": fmt.Sprint(timeout.Milliseconds())})
	return false
}

func (rt *Runtime) stopOneService(name string, rs *runningSession) {
	if rs != nil && rs.cancel != nil {
		rt.shutdownPhase(name, "cancel", func(context.Context) error { rs.cancel(); return nil })
	}
	if dc := rt.dockerClient; dc != nil {
		if id, ok := rt.containerIDs[name]; ok {
			rt.shutdownPhase(name, "docker_stop", func(ctx context.Context) error { return dc.StopContainer(ctx, id, 5) })
			rt.shutdownPhase(name, "docker_remove", func(ctx context.Context) error { return dc.RemoveContainer(ctx, id) })
			delete(rt.containerIDs, name)
		}
	}
	if rs != nil && rs.done != nil {
		rt.shutdownPhase(name, "wait_exit", func(ctx context.Context) error {
			select {
			case <-rs.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	if mgr := rt.proxyMgr; mgr != nil {
		rt.shutdownPhase(name, "proxy_stop", func(context.Context) error { mgr.StopService(name); return nil })
	}
}

func enforceShutdownErrors(tr *TestResult) {
	for _, event := range tr.Events {
		if event.Type != "service_stop_error" {
			continue
		}
		tr.Result = "fail"
		tr.Reason = fmt.Sprintf("TEARDOWN_FAILED: %s (%s): %s", event.Service, event.Fields["phase"], event.Fields["error"])
		return
	}
}

func (rt *Runtime) ShutdownFailed() bool { return rt.shutdownFailed.Load() }

func startupResultError(name string, result *engine.Result) error {
	if result == nil {
		return startupFailure(name, -1, nil)
	}
	return startupFailure(name, result.ExitCode, result.Error)
}
