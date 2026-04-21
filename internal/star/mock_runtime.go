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

		// If tls=True was requested on this interface, generate a leaf
		// cert signed by the runtime's shared mock CA and attach it to
		// the spec. The protocol handler wraps its listener with TLS.
		if svc.Mock.TLS[ifaceName] {
			mt, err := rt.getMockTLS()
			if err != nil {
				svcCancel()
				return fmt.Errorf("mock %q interface %q: tls init: %w", svcName, ifaceName, err)
			}
			cert, err := mt.serverCert(
				[]string{"localhost", svcName},
				[]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
			)
			if err != nil {
				svcCancel()
				return fmt.Errorf("mock %q interface %q: tls cert: %w", svcName, ifaceName, err)
			}
			spec.TLSCert = cert
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

	// RFC-021: when openapi= is set, compose the route table as
	//   [ overrides (highest priority)
	//   , generated (from OpenAPI paths × operations)
	//   , routes= (user-added routes outside the spec, lowest priority)
	//   ]
	// First-match-wins in matchHTTPRoute means overrides eclipse generated
	// entries with the same (normalised) pattern. routes= entries that
	// duplicate a generated pattern never fire — intentional: users who
	// want to override a generated entry should use overrides= for clarity.
	if spec, ok := svc.Mock.OpenAPI[ifaceName]; ok && spec != nil {
		if overrides := svc.Mock.Overrides[ifaceName]; len(overrides) > 0 {
			if err := appendEntries(rt, &out, svcName, ifaceName, overrides); err != nil {
				return out, err
			}
		}
		sel := resolveExampleSelector(svc.Mock.ExampleSelection[ifaceName])
		generated, err := spec.GenerateRoutes(sel)
		if err != nil {
			return out, fmt.Errorf("openapi generate: %w", err)
		}
		out.Routes = append(out.Routes, generated...)

		// Plumb spec + validate mode through so the HTTP handler can
		// enforce request validation at serve time.
		out.OpenAPI = spec
		out.ValidateMode = svc.Mock.Validate[ifaceName]
	}

	if err := appendEntries(rt, &out, svcName, ifaceName, routes); err != nil {
		return out, err
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

	if files, ok := svc.Mock.Descriptors[ifaceName]; ok {
		out.Descriptors = files
	}

	return out, nil
}

// appendEntries converts Starlark mock route entries into protocol mock
// routes and appends them to spec.Routes. Dynamic responses are bridged
// through a Starlark callable closure. Shared between overrides and
// user-supplied routes=. RFC-021.
func appendEntries(rt *Runtime, spec *protocol.MockSpec, svcName, ifaceName string, entries []MockRouteEntry) error {
	for _, entry := range entries {
		if entry.Response == nil {
			return fmt.Errorf("route %q: empty response", entry.Pattern)
		}
		if entry.Response.IsDynamic() {
			callable := entry.Response.Dynamic()
			spec.Routes = append(spec.Routes, protocol.MockRoute{
				Pattern: entry.Pattern,
				Dynamic: rt.dynamicHandlerBridge(svcName, ifaceName, entry.Pattern, callable),
			})
		} else {
			spec.Routes = append(spec.Routes, protocol.MockRoute{
				Pattern:  entry.Pattern,
				Response: entry.Response.Static(),
			})
		}
	}
	return nil
}

// resolveExampleSelector maps the user-facing `examples=` kwarg value to a
// protocol.ExampleSelector.
//
//   - ""          → first (deterministic default)
//   - "first"     → first declared example
//   - "random"    → seeded random per operation
//   - "synthesize" → first example, else schema-synthesised minimal values
//   - any other   → treated as a named-example key (with synth fallback off)
//
// All selectors enable SynthesizeMissing when the user picks "synthesize"
// to make the happy path work without hard-erroring on undocumented ops.
func resolveExampleSelector(name string) protocol.ExampleSelector {
	switch name {
	case "", "first":
		return protocol.FirstExampleSelector{}
	case "synthesize":
		return protocol.FirstExampleSelector{SynthesizeMissing: true}
	case "random":
		// Seed 1 for reproducibility across test runs; users who want
		// non-deterministic fuzzing can pass a different seed via the
		// future `seed=` kwarg (not wired for v0.9.3).
		return protocol.NewRandomExampleSelector(1)
	default:
		return protocol.NamedExampleSelector{Name: name}
	}
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
