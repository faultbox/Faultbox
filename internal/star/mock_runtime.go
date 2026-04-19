package star

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/protocol"
)

// startMockService stands up in-process handlers for every interface on a
// mock service. Each interface runs in its own goroutine; they all share a
// single cancellation context so stopServices can tear them down.
func (rt *Runtime) startMockService(ctx context.Context, svcName string, svc *ServiceDef) error {
	if svc.Mock == nil {
		return fmt.Errorf("startMockService: %q has nil Mock config", svcName)
	}

	svcCtx, svcCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var wg sync.WaitGroup

	for ifaceName, iface := range svc.Interfaces {
		p, ok := protocol.Get(iface.Protocol)
		if !ok {
			svcCancel()
			return fmt.Errorf("mock %q: unknown protocol %q", svcName, iface.Protocol)
		}
		mh, ok := p.(protocol.MockHandler)
		if !ok {
			svcCancel()
			return fmt.Errorf("mock %q: protocol %q has no mock handler", svcName, iface.Protocol)
		}

		spec, err := rt.buildMockSpec(svcName, ifaceName, svc)
		if err != nil {
			svcCancel()
			return fmt.Errorf("mock %q interface %q: %w", svcName, ifaceName, err)
		}

		addr := fmt.Sprintf("127.0.0.1:%d", iface.Port)
		emit := rt.mockEmitter(svcName, ifaceName)

		wg.Add(1)
		go func(name, addr string) {
			defer wg.Done()
			if err := mh.ServeMock(svcCtx, addr, spec, emit); err != nil {
				rt.log.Error("mock handler failed",
					slog.String("service", svcName),
					slog.String("interface", name),
					slog.String("error", err.Error()))
			}
		}(ifaceName, addr)

		if err := waitMockReady(ctx, iface.Protocol, addr, 3*time.Second); err != nil {
			svcCancel()
			wg.Wait()
			return fmt.Errorf("mock %q interface %q not ready: %w", svcName, ifaceName, err)
		}

		rt.log.Info("mock service listening",
			slog.String("service", svcName),
			slog.String("interface", ifaceName),
			slog.String("addr", addr))
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	rt.sessions[svcName] = &runningSession{
		session: nil,
		cancel:  svcCancel,
		done:    mockDoneChan(done),
	}

	rt.events.Emit("service_started", svcName, map[string]string{"kind": "mock"})
	rt.events.Emit("service_ready", svcName, map[string]string{"kind": "mock"})
	return nil
}

// buildMockSpec translates a per-interface slice of MockRouteEntry into the
// protocol-level MockSpec. It wires dynamic handlers through a closure that
// invokes the Starlark callable on a fresh thread per request.
func (rt *Runtime) buildMockSpec(svcName, ifaceName string, svc *ServiceDef) (protocol.MockSpec, error) {
	routes := svc.Mock.Routes[ifaceName]
	out := protocol.MockSpec{
		Routes: make([]protocol.MockRoute, 0, len(routes)),
	}

	for _, entry := range routes {
		if entry.Response == nil {
			return out, fmt.Errorf("route %q: empty response", entry.Pattern)
		}
		if entry.Response.IsDynamic() {
			callable := entry.Response.Dynamic()
			out.Routes = append(out.Routes, protocol.MockRoute{
				Pattern: entry.Pattern,
				Dynamic: rt.dynamicHandlerBridge(svcName, ifaceName, entry.Pattern, callable),
			})
		} else {
			out.Routes = append(out.Routes, protocol.MockRoute{
				Pattern:  entry.Pattern,
				Response: entry.Response.Static(),
			})
		}
	}

	if def, ok := svc.Mock.Default[ifaceName]; ok && def != nil {
		if def.IsDynamic() {
			return out, fmt.Errorf("default response cannot be dynamic() — use a static response")
		}
		out.Default = def.Static()
	}

	if cfg, ok := svc.Mock.Config[ifaceName]; ok {
		out.Config = cfg
	}

	return out, nil
}

// dynamicHandlerBridge wraps a Starlark callable so it satisfies
// protocol.DynamicFn. Each invocation runs on a fresh Starlark thread; the
// runtime-wide mutex serializes concurrent invocations to keep Starlark
// state modifications safe (though mock handlers should not mutate shared
// state in practice).
func (rt *Runtime) dynamicHandlerBridge(svcName, ifaceName, pattern string, fn starlark.Callable) protocol.DynamicFn {
	return func(req protocol.MockRequest) (*protocol.MockResponse, error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()

		thread := &starlark.Thread{Name: fmt.Sprintf("mock-%s-%s-%s", svcName, ifaceName, pattern)}
		reqDict := toStarlarkRequest(req)
		result, err := starlark.Call(thread, fn, starlark.Tuple{reqDict}, nil)
		if err != nil {
			return nil, fmt.Errorf("dynamic handler %s: %w", pattern, err)
		}
		return starlarkResponseToProtocol(result)
	}
}

// mockEmitter returns an emit callback that forwards mock-handler events
// into the runtime event log. The emitter is thread-safe.
func (rt *Runtime) mockEmitter(svcName, ifaceName string) protocol.MockEmitter {
	return func(op string, fields map[string]string) {
		if fields == nil {
			fields = make(map[string]string, 2)
		}
		fields["interface"] = ifaceName
		fields["kind"] = "mock"
		fields["op"] = op
		rt.events.Emit("mock."+op, svcName, fields)
	}
}

// waitMockReady probes addr until the listener is bound or the deadline
// passes. TCP-based protocols (tcp, http, http2) use a dial probe; UDP is
// probed by trying to re-bind the same port — if the bind fails with
// EADDRINUSE, the mock has claimed it. Unknown protocols fall back to TCP
// (safest default; fails fast if wrong).
func waitMockReady(ctx context.Context, proto, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mockListenerUp(proto, addr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return fmt.Errorf("listener on %s (proto=%s) not ready after %s", addr, proto, timeout)
}

func mockListenerUp(proto, addr string) bool {
	switch proto {
	case "udp":
		// UDP is connectionless — there's no TCP handshake to probe. The
		// bind-test trick (try to re-bind the same port) is unreliable on
		// platforms that default to SO_REUSEADDR/SO_REUSEPORT. The mock's
		// net.ListenPacket returns synchronously inside the goroutine's
		// first line, so "yield and assume ready" is accurate in practice;
		// a short sleep to give the goroutine time to run is sufficient.
		time.Sleep(25 * time.Millisecond)
		return true
	default:
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		return false
	}
}

// mockDoneChan adapts a struct{} completion channel to the *engine.Result
// channel type runningSession.done expects. stopServices() only selects on
// it for synchronization and never reads the value, so closing the channel
// without sending anything is sufficient.
func mockDoneChan(done <-chan struct{}) chan *engine.Result {
	out := make(chan *engine.Result, 1)
	go func() {
		<-done
		close(out)
	}()
	return out
}
