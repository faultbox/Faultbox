package star

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/config"
	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/logging"
)

// TestResult captures the outcome of one test function.
type TestResult struct {
	Name       string  `json:"name"`
	Result     string  `json:"result"` // "pass" or "fail"
	Reason     string  `json:"reason,omitempty"`
	Seed       uint64  `json:"seed"`
	DurationMs int64   `json:"duration_ms"`
	Events     []Event `json:"events,omitempty"`
}

// SuiteResult captures the outcome of all test functions.
type SuiteResult struct {
	DurationMs int64        `json:"duration_ms"`
	Tests      []TestResult `json:"tests"`
	Pass       int          `json:"pass"`
	Fail       int          `json:"fail"`
}

// Runtime is the Starlark execution environment.
type Runtime struct {
	log      *slog.Logger
	events   *EventLog
	eng      *engine.Engine

	// Service registry — populated during .star file load.
	mu       sync.Mutex
	services map[string]*ServiceDef
	order    []string // dependency order

	// Running sessions — populated during test execution.
	sessions map[string]*runningSession

	// Active faults per service — modified by fault_start/fault_stop.
	faultsMu sync.Mutex
	faults   map[string]map[string]*FaultDef // service -> syscall -> fault

	// Starlark globals — test functions discovered after load.
	globals starlark.StringDict

	// Seed for deterministic probabilistic faults (nil = random).
	seed *uint64
}

type runningSession struct {
	session *engine.Session
	cancel  context.CancelFunc
	done    chan *engine.Result
}

// New creates a new Starlark runtime.
func New(logger *slog.Logger) *Runtime {
	return &Runtime{
		log:      logging.WithComponent(logger, "starlark"),
		events:   NewEventLog(),
		eng:      engine.New(logger),
		services: make(map[string]*ServiceDef),
		sessions: make(map[string]*runningSession),
		faults:   make(map[string]map[string]*FaultDef),
	}
}

// LoadFile executes a .star file, populating the service registry and
// discovering test_* functions.
func (rt *Runtime) LoadFile(path string) error {
	thread := &starlark.Thread{Name: "load"}

	globals, err := starlark.ExecFile(thread, path, nil, rt.builtins())
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}

	rt.globals = globals
	return nil
}

// LoadString executes Starlark source from a string (for testing).
func (rt *Runtime) LoadString(name, src string) error {
	thread := &starlark.Thread{Name: "load"}
	globals, err := starlark.ExecFile(thread, name, src, rt.builtins())
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	rt.globals = globals
	return nil
}

// DiscoverTests returns sorted test function names (test_* globals).
func (rt *Runtime) DiscoverTests() []string {
	var names []string
	for name, val := range rt.globals {
		if strings.HasPrefix(name, "test_") {
			if _, ok := val.(starlark.Callable); ok {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// Services returns the registered services in dependency order.
func (rt *Runtime) Services() []*ServiceDef {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	result := make([]*ServiceDef, 0, len(rt.order))
	for _, name := range rt.order {
		result = append(result, rt.services[name])
	}
	return result
}

// RunConfig controls test execution parameters.
type RunConfig struct {
	Filter   string  // run only matching test (empty = all)
	Seed     *uint64 // explicit seed (nil = auto-increment from 0)
	Runs     int     // number of runs per test (0 or 1 = single run)
	FailOnly bool    // only keep failing test results
}

// RunAll executes all (or filtered) test functions.
func (rt *Runtime) RunAll(ctx context.Context, cfg RunConfig) (*SuiteResult, error) {
	start := time.Now()
	tests := rt.DiscoverTests()

	runs := cfg.Runs
	if runs <= 0 {
		runs = 1
	}

	suite := &SuiteResult{
		Tests: make([]TestResult, 0, len(tests)*runs),
	}

	for _, name := range tests {
		if cfg.Filter != "" && name != "test_"+cfg.Filter && name != cfg.Filter {
			continue
		}

		for run := 0; run < runs; run++ {
			// Determine seed for this run.
			var seed uint64
			if cfg.Seed != nil {
				seed = *cfg.Seed
			} else {
				seed = uint64(run)
			}
			rt.seed = &seed

			runLabel := name
			if runs > 1 {
				runLabel = fmt.Sprintf("%s [seed=%d]", name, seed)
			}
			rt.log.Info("running test", slog.String("test", runLabel))

			tr := rt.RunTest(ctx, name)
			tr.Seed = seed

			if runs > 1 {
				rt.log.Info("test completed",
					slog.String("test", name),
					slog.String("result", tr.Result),
					slog.Uint64("seed", seed),
					slog.Int("run", run+1),
					slog.Int("of", runs),
				)
			} else {
				rt.log.Info("test completed",
					slog.String("test", name),
					slog.String("result", tr.Result),
					slog.Int64("duration_ms", tr.DurationMs),
				)
			}

			if tr.Result == "pass" {
				suite.Pass++
				if !cfg.FailOnly {
					suite.Tests = append(suite.Tests, tr)
				}
			} else {
				suite.Fail++
				suite.Tests = append(suite.Tests, tr)
				// In multi-run mode, stop this test on first failure.
				if runs > 1 {
					rt.log.Warn("failure found, stopping runs for this test",
						slog.String("test", name),
						slog.Uint64("seed", seed),
						slog.Int("run", run+1),
					)
					break
				}
			}
		}
	}

	suite.DurationMs = time.Since(start).Milliseconds()
	return suite, nil
}

// RunTest executes a single test function with fresh services.
func (rt *Runtime) RunTest(ctx context.Context, name string) TestResult {
	start := time.Now()

	fn, ok := rt.globals[name].(starlark.Callable)
	if !ok {
		return TestResult{
			Name: name, Result: "fail",
			Reason:     fmt.Sprintf("test %q not found or not callable", name),
			DurationMs: time.Since(start).Milliseconds(),
		}
	}

	// Reset event log for this test.
	rt.events.Reset()

	// Wait for ports to be free.
	rt.waitPortsFree(10 * time.Second)

	// Start services.
	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := rt.startServices(testCtx); err != nil {
		rt.stopServices()
		return TestResult{
			Name: name, Result: "fail",
			Reason:     fmt.Sprintf("failed to start services: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
			Events:     rt.events.Events(),
		}
	}

	// Run the test function.
	thread := &starlark.Thread{Name: name}
	_, err := starlark.Call(thread, fn, nil, nil)

	// Stop services.
	rt.stopServices()

	events := rt.events.Events()

	if err != nil {
		return TestResult{
			Name: name, Result: "fail",
			Reason:     err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
			Events:     events,
		}
	}

	return TestResult{
		Name: name, Result: "pass",
		DurationMs: time.Since(start).Milliseconds(),
		Events:     events,
	}
}

// registerService adds a service to the registry.
func (rt *Runtime) registerService(svc *ServiceDef) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	svc.rt = rt
	rt.services[svc.Name] = svc
	// Rebuild dependency order.
	rt.order = rt.dependencyOrder()
}

// dependencyOrder returns service names in topological order.
func (rt *Runtime) dependencyOrder() []string {
	cfgServices := make(map[string]config.ServiceConfig)
	for name, svc := range rt.services {
		cfgServices[name] = config.ServiceConfig{
			DependsOn: svc.DependsOn,
		}
	}
	order, err := config.DependencyOrder(cfgServices)
	if err != nil {
		// Fall back to insertion order on error.
		var names []string
		for name := range rt.services {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	return order
}

// startServices starts all registered services in dependency order.
func (rt *Runtime) startServices(ctx context.Context) error {
	rt.mu.Lock()
	order := make([]string, len(rt.order))
	copy(order, rt.order)
	rt.mu.Unlock()

	for _, svcName := range order {
		svc := rt.services[svcName]

		// Build env vars with auto-injection.
		envVars := rt.buildEnv(svc)

		// Collect all faultable syscalls for pre-installed seccomp filter.
		var faultRules []engine.FaultRule
		faultRules = append(faultRules, rt.preinstallRules()...)

		ns := engine.NamespaceConfig{
			PID:   true,
			Mount: true,
			User:  true,
		}

		// Wire syscall events into the central event log.
		svcNameCopy := svcName // capture for closure
		onSyscall := func(evt engine.SyscallEvent) {
			fields := map[string]string{
				"syscall":  evt.Syscall,
				"pid":      fmt.Sprintf("%d", evt.PID),
				"decision": evt.Decision,
			}
			if evt.Path != "" {
				fields["path"] = evt.Path
			}
			if evt.Latency > 0 {
				fields["latency_ms"] = fmt.Sprintf("%d", evt.Latency.Milliseconds())
			}
			rt.events.Emit("syscall", svcNameCopy, fields)

			// Causal merge: when a service makes a network call (connect/sendto)
			// that is allowed, merge the target service's clock into this service.
			if (evt.Syscall == "connect" || evt.Syscall == "sendto") &&
				strings.HasPrefix(evt.Decision, "allow") {
				rt.mergeClocksForNetworkCall(svcNameCopy)
			}
		}

		sessCfg := engine.SessionConfig{
			Binary:     svc.Binary,
			Args:       nil,
			Env:        envVars,
			Stdout:     os.Stdout,
			Stderr:     os.Stderr,
			Namespaces: ns,
			FaultRules: faultRules,
			OnSyscall:  onSyscall,
			Seed:       rt.seed,
		}

		svcLog := rt.log.With(slog.String("service", svcName))
		session, err := engine.NewSession(sessCfg, svcLog)
		if err != nil {
			return fmt.Errorf("create session for %q: %w", svcName, err)
		}
		session.Service = svcName

		svcCtx, svcCancel := context.WithCancel(ctx)
		done := make(chan *engine.Result, 1)
		go func() {
			r, _ := session.Run(svcCtx)
			done <- r
		}()

		rt.sessions[svcName] = &runningSession{
			session: session,
			cancel:  svcCancel,
			done:    done,
		}

		rt.events.Emit("service_started", svcName, nil)

		// Bind InterfaceRef.runtime for step execution.
		for _, iface := range svc.Interfaces {
			// Ensure InterfaceRefs have runtime pointer.
			_ = iface // bindings happen through Attr calls
		}

		// Wait for healthcheck.
		if svc.Healthcheck != nil {
			timeout := svc.Healthcheck.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Second
			}
			if err := waitReady(ctx, svc.Healthcheck.Test, timeout); err != nil {
				return fmt.Errorf("service %q not ready: %w", svcName, err)
			}
			rt.events.Emit("service_ready", svcName, nil)
			svcLog.Info("service ready", slog.String("check", svc.Healthcheck.Test))
		}
	}
	return nil
}

// stopServices stops all running services.
func (rt *Runtime) stopServices() {
	for _, rs := range rt.sessions {
		rs.cancel()
	}
	for _, rs := range rt.sessions {
		select {
		case <-rs.done:
		case <-time.After(5 * time.Second):
		}
	}
	rt.sessions = make(map[string]*runningSession)

	// Clear active faults.
	rt.faultsMu.Lock()
	rt.faults = make(map[string]map[string]*FaultDef)
	rt.faultsMu.Unlock()
}

// buildEnv creates the environment for a service with auto-injection.
func (rt *Runtime) buildEnv(svc *ServiceDef) []string {
	env := make(map[string]string)

	// Auto-inject FAULTBOX_* vars for all services.
	for name, s := range rt.services {
		upper := strings.ToUpper(name)
		for ifName, iface := range s.Interfaces {
			ifUpper := strings.ToUpper(ifName)
			prefix := fmt.Sprintf("FAULTBOX_%s_%s", upper, ifUpper)
			env[prefix+"_HOST"] = "localhost"
			env[prefix+"_PORT"] = fmt.Sprintf("%d", iface.Port)
			env[prefix+"_ADDR"] = fmt.Sprintf("localhost:%d", iface.Port)
		}
	}

	// User-defined env (templates already resolved in Starlark via .addr).
	for k, v := range svc.Env {
		env[k] = v
	}

	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}

// preinstallRules returns empty fault rules for common syscalls so the seccomp
// filter is pre-installed. Actual faults are applied dynamically.
func (rt *Runtime) preinstallRules() []engine.FaultRule {
	// Pre-install filter for common faultable syscalls.
	// Rules with Probability=0 will never fire but ensure the syscall is intercepted.
	syscalls := []string{"write", "read", "connect", "openat", "fsync", "sendto", "recvfrom", "writev"}
	var rules []engine.FaultRule
	for _, sc := range syscalls {
		rules = append(rules, engine.FaultRule{
			Syscall:     sc,
			Action:      engine.ActionDeny,
			Probability: 0, // never fires — just pre-installs the seccomp filter
		})
	}
	return rules
}

// applyFaults sets fault rules for a service's running session.
func (rt *Runtime) applyFaults(svcName string, faults map[string]*FaultDef) error {
	rt.faultsMu.Lock()
	rt.faults[svcName] = faults
	rt.faultsMu.Unlock()

	// Convert to engine.FaultRule and inject into running session.
	rs, ok := rt.sessions[svcName]
	if !ok {
		return fmt.Errorf("service %q is not running", svcName)
	}

	var rules []engine.FaultRule
	for syscall, fd := range faults {
		rule := engine.FaultRule{
			Syscall:     syscall,
			Probability: fd.Probability,
		}
		switch fd.Action {
		case "delay":
			rule.Action = engine.ActionDelay
			rule.Delay = fd.Delay
		case "deny":
			rule.Action = engine.ActionDeny
			parsed, err := engine.ParseFaultRule(syscall + "=" + fd.Errno + ":100%")
			if err == nil {
				rule.Errno = parsed.Errno
			}
		}
		rules = append(rules, rule)
	}

	rs.session.SetDynamicFaultRules(rules)
	rt.events.Emit("fault_applied", svcName, nil)
	return nil
}

// removeFaults clears fault rules for a service.
func (rt *Runtime) removeFaults(svcName string) {
	rt.faultsMu.Lock()
	delete(rt.faults, svcName)
	rt.faultsMu.Unlock()

	if rs, ok := rt.sessions[svcName]; ok {
		rs.session.ClearDynamicFaultRules()
	}
	rt.events.Emit("fault_removed", svcName, nil)
}

// mergeClocksForNetworkCall merges all other services' clocks into the calling
// service. This is a conservative approximation: when service A makes a network
// call, we merge all known service clocks into A since we don't yet resolve the
// target port to a specific service at the seccomp level.
func (rt *Runtime) mergeClocksForNetworkCall(svcName string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for name := range rt.services {
		if name != svcName {
			rt.events.MergeClock(svcName, name)
		}
	}
}

// executeStep runs an HTTP or TCP step against a running service.
func (rt *Runtime) executeStep(thread *starlark.Thread, ref *InterfaceRef, method string, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	addr := fmt.Sprintf("localhost:%d", ref.Interface.Port)
	targetSvc := ref.Service.Name

	// Emit step_send event from test driver — shows request going out.
	rt.events.Emit("step_send", "faultbox", map[string]string{
		"target":    targetSvc,
		"method":    method,
		"interface": ref.Interface.Name,
		"protocol":  ref.Interface.Protocol,
	})

	var result starlark.Value
	var err error

	switch ref.Interface.Protocol {
	case "http":
		result, err = rt.executeHTTPStep(addr, method, kwargs)
	case "tcp":
		result, err = rt.executeTCPStep(addr, method, kwargs)
	default:
		return nil, fmt.Errorf("unsupported protocol %q", ref.Interface.Protocol)
	}

	// After a step completes, merge target service's clock into the test driver.
	// This records the causal dependency: test observed targetSvc's state.
	rt.events.MergeClock("faultbox", targetSvc)

	// Emit step_recv event — shows response received, with merged clock.
	rt.events.Emit("step_recv", "faultbox", map[string]string{
		"target": targetSvc,
		"method": method,
	})

	return result, err
}

func (rt *Runtime) executeHTTPStep(addr, method string, kwargs []starlark.Tuple) (starlark.Value, error) {
	stepArgs := make(map[string]any)
	stepArgs["path"] = "/"

	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "path":
			s, _ := starlark.AsString(kv[1])
			stepArgs["path"] = s
		case "body":
			s, _ := starlark.AsString(kv[1])
			stepArgs["body"] = s
		case "headers":
			dict, ok := kv[1].(*starlark.Dict)
			if ok {
				headers := make(map[string]any)
				for _, item := range dict.Items() {
					k, _ := starlark.AsString(item[0])
					v, _ := starlark.AsString(item[1])
					headers[k] = v
				}
				stepArgs["headers"] = headers
			}
		}
	}

	result, err := engine.RunHTTPStep(context.Background(), addr, strings.ToUpper(method), stepArgs)
	if err != nil {
		return nil, err
	}

	return &Response{
		Status:     result.StatusCode,
		Body:       result.Body,
		DurationMs: result.DurationMs,
		Ok:         result.Success,
		Error:      result.Error,
	}, nil
}

func (rt *Runtime) executeTCPStep(addr, method string, kwargs []starlark.Tuple) (starlark.Value, error) {
	if method != "send" {
		return nil, fmt.Errorf("TCP only supports 'send', got %q", method)
	}

	stepArgs := make(map[string]any)
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "data":
			s, _ := starlark.AsString(kv[1])
			stepArgs["data"] = s
		}
	}

	result, err := engine.RunTCPStep(context.Background(), addr, "send", stepArgs)
	if err != nil {
		return nil, err
	}

	// For TCP send, return the response body as a string (simpler API).
	if result.Success {
		return starlark.String(result.Body), nil
	}
	return nil, fmt.Errorf("tcp send failed: %s", result.Error)
}

// waitReady polls a readiness check.
func waitReady(ctx context.Context, check string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if strings.HasPrefix(check, "tcp://") {
			addr := strings.TrimPrefix(check, "tcp://")
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				conn.Close()
				return nil
			}
		} else if strings.HasPrefix(check, "http://") || strings.HasPrefix(check, "https://") {
			client := &http.Client{Timeout: time.Second}
			resp, err := client.Get(check)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					return nil
				}
			}
		}

		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("readiness check %q timed out after %s", check, timeout)
}

// waitPortsFree waits for service ports to be available.
func (rt *Runtime) waitPortsFree(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allFree := true
		for _, svc := range rt.services {
			for _, iface := range svc.Interfaces {
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", iface.Port), 100*time.Millisecond)
				if err == nil {
					conn.Close()
					allFree = false
					break
				}
			}
			if !allFree {
				break
			}
		}
		if allFree {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
