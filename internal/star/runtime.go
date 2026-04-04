package star

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/config"
	"github.com/faultbox/Faultbox/internal/container"
	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/seccomp"
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

	// Container support.
	dockerClient   *container.Client      // lazy-initialized Docker client
	networkID      string                 // Faultbox Docker network ID
	containerIDs   map[string]string      // service name → container ID (for cleanup)
	baseDir        string                 // directory of the loaded .star file (for build= paths)
	sourceText     string                 // raw .star source for syscall scanning

	// Seed for deterministic probabilistic faults (nil = random).
	seed *uint64

	// Virtual time: skip fault delays, advance virtual clock instead.
	virtualTime bool

	// Exploration mode: "all", "sample", or "".
	exploreMode string
	explorePerm int // current permutation index for explore mode

	// Services excluded from interleaving control in parallel().
	nondetServices map[string]bool

	// Monitor errors — collected during test execution, checked after test.
	monitorMu     sync.Mutex
	monitorErrors []error
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

	// Store base directory for resolving relative paths (build=, volumes=).
	absPath, err := filepath.Abs(path)
	if err == nil {
		rt.baseDir = filepath.Dir(absPath)
	}

	// Read source for syscall scanning.
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	rt.sourceText = string(src)

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
	rt.sourceText = src
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
	Filter      string  // run only matching test (empty = all)
	Seed        *uint64 // explicit seed (nil = auto-increment from 0)
	Runs        int     // number of runs per test (0 or 1 = single run)
	FailOnly    bool    // only keep failing test results
	VirtualTime bool    // enable virtual time (skip fault delays)
	ExploreMode string  // "all" (exhaustive), "sample" (random), or "" (off)
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
			rt.virtualTime = cfg.VirtualTime
			rt.exploreMode = cfg.ExploreMode
			rt.explorePerm = run

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

	// Warn if filter matched no tests.
	if cfg.Filter != "" && len(suite.Tests) == 0 {
		available := strings.Join(tests, ", ")
		fmt.Fprintf(os.Stderr, "WARNING: no test matched filter %q (available: %s)\n", cfg.Filter, available)
	}

	// Clean up Docker resources after all tests.
	rt.cleanup()

	return suite, nil
}

// cleanup releases Docker resources (network, client) after all tests complete.
func (rt *Runtime) cleanup() {
	if rt.dockerClient != nil {
		ctx := context.Background()
		if rt.networkID != "" {
			rt.dockerClient.RemoveNetwork(ctx, rt.networkID)
			rt.networkID = ""
		}
		rt.dockerClient.Close()
		rt.dockerClient = nil
	}
	// Final socket cleanup.
	socketBase := filepath.Join(os.TempDir(), "faultbox-sockets")
	os.RemoveAll(socketBase)
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

	// Reset event log and monitors for this test.
	rt.events.Reset()
	rt.events.ClearSubscribers()
	rt.monitorMu.Lock()
	rt.monitorErrors = nil
	rt.monitorMu.Unlock()

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

	// Check monitor errors.
	rt.monitorMu.Lock()
	monErrs := rt.monitorErrors
	rt.monitorMu.Unlock()
	if len(monErrs) > 0 {
		return TestResult{
			Name: name, Result: "fail",
			Reason:     fmt.Sprintf("monitor violation: %v", monErrs[0]),
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

		var err error
		if svc.IsContainer() {
			err = rt.startContainerService(ctx, svcName, svc)
		} else {
			err = rt.startBinaryService(ctx, svcName, svc)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// startBinaryService starts a service from a local binary (PoC 1 path).
func (rt *Runtime) startBinaryService(ctx context.Context, svcName string, svc *ServiceDef) error {
	envVars := rt.buildEnv(svc)

	var faultRules []engine.FaultRule
	if svcRules := rt.requiredFaultRulesForService(svcName); svcRules != nil {
		faultRules = append(faultRules, svcRules...)
	}

	ns := engine.NamespaceConfig{PID: true, Mount: true, User: true}
	onSyscall := rt.makeSyscallCallback(svcName)

	sessCfg := engine.SessionConfig{
		Binary:             svc.Binary,
		Args:               svc.Args,
		Env:                envVars,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
		Namespaces:         ns,
		FaultRules:         faultRules,
		OnSyscall:          onSyscall,
		Seed:               rt.seed,
		VirtualTime:        rt.virtualTime,
		ExternalListenerFd: -1, // not external
	}

	return rt.launchSession(ctx, svcName, svc, sessCfg)
}

// startContainerService starts a service from a Docker container image (PoC 2 path).
func (rt *Runtime) startContainerService(ctx context.Context, svcName string, svc *ServiceDef) error {
	// Lazy-init Docker client and network.
	if rt.dockerClient == nil {
		dc, err := container.NewClient(ctx, rt.log)
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		rt.dockerClient = dc
		rt.containerIDs = make(map[string]string)

		netID, err := dc.EnsureNetwork(ctx)
		if err != nil {
			return fmt.Errorf("docker network: %w", err)
		}
		rt.networkID = netID
	}

	// Resolve image: either pull or build from Dockerfile.
	imageName := svc.Image
	if svc.Build != "" {
		imageName = fmt.Sprintf("faultbox-%s:latest", svc.Name)
		buildCtx := svc.Build
		if !filepath.IsAbs(buildCtx) && rt.baseDir != "" {
			buildCtx = filepath.Join(rt.baseDir, buildCtx)
		}
		if err := rt.dockerClient.BuildImage(ctx, buildCtx, imageName); err != nil {
			return fmt.Errorf("build image for %s: %w", svc.Name, err)
		}
	}

	// Resolve ports from interfaces.
	ports := make(map[int]int)
	for _, iface := range svc.Interfaces {
		ports[iface.Port] = 0 // 0 = let Docker pick host port
	}

	// Resolve syscall numbers for seccomp filter (per-service).
	var syscallNrs []uint32
	svcRules := rt.requiredFaultRulesForService(svcName)
	for _, r := range svcRules {
		nr := seccomp.SyscallNumber(r.Syscall)
		if nr >= 0 {
			syscallNrs = append(syscallNrs, uint32(nr))
		}
	}

	// Build container env (use container hostnames for inter-service refs).
	envVars := rt.buildContainerEnv(svc)

	// Find the shim binary path.
	shimPath := rt.findShimPath()

	// Launch container.
	result, err := container.Launch(ctx, rt.dockerClient, container.LaunchConfig{
		Name:       svcName,
		Image:      imageName,
		Env:        envVars,
		Ports:      ports,
		Volumes:    svc.Volumes,
		SyscallNrs: syscallNrs,
		ShimPath:   shimPath,
		NetworkID:  rt.networkID,
		SkipPull:   svc.Build != "", // locally built images don't need pull
	}, rt.log)
	if err != nil {
		return fmt.Errorf("launch container %q: %w", svcName, err)
	}
	rt.containerIDs[svcName] = result.ContainerID

	// Update interface ports to actual host-mapped ports (for healthchecks + steps).
	for _, iface := range svc.Interfaces {
		if hp, ok := result.HostPorts[iface.Port]; ok {
			iface.HostPort = hp
		}
	}

	// If no seccomp filter (no fault rules), skip session — just wait for healthcheck.
	if result.ListenerFd < 0 {
		rt.log.Info("container running (no seccomp)", slog.String("service", svcName))
		rt.events.Emit("service_started", svcName, nil)

		if svc.Healthcheck != nil {
			timeout := svc.Healthcheck.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Second
			}
			hcTest := rt.resolveHealthcheck(svc)
			if err := waitReady(ctx, hcTest, timeout); err != nil {
				return fmt.Errorf("service %q not ready: %w", svcName, err)
			}
			rt.events.Emit("service_ready", svcName, nil)
			rt.log.Info("service ready", slog.String("service", svcName), slog.String("check", hcTest))
		}
		return nil
	}

	// Create session with external listener fd (per-service rules).
	onSyscall := rt.makeSyscallCallback(svcName)
	faultRules := svcRules // already computed above

	sessCfg := engine.SessionConfig{
		FaultRules:         faultRules,
		OnSyscall:          onSyscall,
		Seed:               rt.seed,
		VirtualTime:        rt.virtualTime,
		ExternalListenerFd: result.ListenerFd,
		ExternalPID:        result.HostPID,
	}

	return rt.launchSession(ctx, svcName, svc, sessCfg)
}

// makeSyscallCallback creates the OnSyscall callback for a service.
func (rt *Runtime) makeSyscallCallback(svcName string) func(engine.SyscallEvent) {
	return func(evt engine.SyscallEvent) {
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
		rt.events.Emit("syscall", svcName, fields)

		if (evt.Syscall == "connect" || evt.Syscall == "sendto") &&
			strings.HasPrefix(evt.Decision, "allow") {
			rt.mergeClocksForNetworkCall(svcName)
		}
	}
}

// launchSession creates and starts a session, waits for healthcheck.
func (rt *Runtime) launchSession(ctx context.Context, svcName string, svc *ServiceDef, sessCfg engine.SessionConfig) error {
	svcLog := rt.log.With(slog.String("service", svcName))
	session, err := engine.NewSession(sessCfg, svcLog)
	if err != nil {
		return fmt.Errorf("create session for %q: %w", svcName, err)
	}
	session.Service = svcName

	// Sessions use their own context (not tied to startServices/test context).
	// They are explicitly cancelled by stopServices() via svcCancel.
	svcCtx, svcCancel := context.WithCancel(context.Background())
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

	// Wait for healthcheck.
	if svc.Healthcheck != nil {
		timeout := svc.Healthcheck.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		// For containers with mapped ports, adjust the healthcheck URL.
		hcTest := rt.resolveHealthcheck(svc)
		if err := waitReady(ctx, hcTest, timeout); err != nil {
			return fmt.Errorf("service %q not ready: %w", svcName, err)
		}
		rt.events.Emit("service_ready", svcName, nil)
		svcLog.Info("service ready", slog.String("check", hcTest))
	}
	return nil
}

// resolveHealthcheck returns the healthcheck URL, adjusting for container port mapping.
func (rt *Runtime) resolveHealthcheck(svc *ServiceDef) string {
	if svc.Healthcheck == nil {
		return ""
	}
	test := svc.Healthcheck.Test
	if !svc.IsContainer() {
		return test
	}
	// For containers, replace the declared port with the actual host-mapped port.
	for _, iface := range svc.Interfaces {
		if iface.HostPort > 0 && iface.HostPort != iface.Port {
			test = strings.ReplaceAll(test,
				fmt.Sprintf(":%d", iface.Port),
				fmt.Sprintf(":%d", iface.HostPort))
		}
	}
	return test
}

// buildContainerEnv builds environment variables for a container service.
// Uses container hostnames for inter-service references.
func (rt *Runtime) buildContainerEnv(svc *ServiceDef) []string {
	var env []string
	// Auto-inject FAULTBOX_* for all registered services.
	for _, otherName := range rt.order {
		other := rt.services[otherName]
		for _, iface := range other.Interfaces {
			prefix := fmt.Sprintf("FAULTBOX_%s_%s", strings.ToUpper(otherName), strings.ToUpper(iface.Name))
			// For container env: use container hostname (service name).
			host := otherName
			if !other.IsContainer() {
				host = "localhost"
			}
			env = append(env,
				fmt.Sprintf("%s_HOST=%s", prefix, host),
				fmt.Sprintf("%s_PORT=%d", prefix, iface.Port),
				fmt.Sprintf("%s_ADDR=%s:%d", prefix, host, iface.Port),
			)
		}
	}
	// User-defined env.
	for k, v := range svc.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// findShimPath locates the faultbox-shim binary.
func (rt *Runtime) findShimPath() string {
	// Try common locations.
	candidates := []string{
		"/tmp/faultbox-shim",
		"bin/linux-arm64/faultbox-shim",
		"/host-home/git/Faultbox/bin/linux-arm64/faultbox-shim",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			// Docker bind mounts require absolute paths.
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	// Fallback: assume it's alongside the faultbox binary.
	exe, _ := os.Executable()
	if exe != "" {
		dir := filepath.Dir(exe)
		return filepath.Join(dir, "faultbox-shim")
	}
	return "faultbox-shim"
}

// stopServices stops all running services and cleans up containers.
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

	// Stop and remove Docker containers.
	if rt.dockerClient != nil {
		ctx := context.Background()
		for name, cid := range rt.containerIDs {
			rt.log.Debug("stopping container", slog.String("name", name))
			rt.dockerClient.StopContainer(ctx, cid, 5)
			rt.dockerClient.RemoveContainer(ctx, cid)
		}
		rt.containerIDs = make(map[string]string)
	}

	// Clean up socket directories.
	socketBase := filepath.Join(os.TempDir(), "faultbox-sockets")
	os.RemoveAll(socketBase)

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

// expandSyscallFamily maps a user-facing fault keyword to all underlying syscalls.
// For example, "write" maps to write, writev, pwrite64 since real applications
// (e.g., Postgres) may use any of these for disk I/O.
func expandSyscallFamily(name string) []string {
	switch name {
	case "write":
		return []string{"write", "writev", "pwrite64"}
	case "read":
		return []string{"read", "readv", "pread64"}
	case "open":
		return []string{"open", "openat"}
	case "fsync":
		return []string{"fsync", "fdatasync"}
	case "sendto":
		return []string{"sendto", "sendmsg"}
	case "recvfrom":
		return []string{"recvfrom", "recvmsg"}
	default:
		return []string{name}
	}
}

// faultableSyscalls is the set of syscall names that can appear as fault keywords.
var faultableSyscalls = []string{
	"write", "read", "connect", "openat", "fsync",
	"sendto", "recvfrom", "writev", "readv", "close",
	"pwrite64", "pread64", "fdatasync", "sendmsg", "recvmsg",
}

// requiredSyscalls scans the loaded Starlark source to determine which syscalls
// tests actually reference in fault()/fault_start()/partition() calls.
// Only these syscalls are installed in the seccomp filter, avoiding the overhead
// of intercepting irrelevant syscalls (e.g., openat during library loading).
func (rt *Runtime) requiredSyscalls() []string {
	src := rt.sourceText
	found := make(map[string]bool)

	// Scan for fault keywords: "write=deny", "write=delay", "connect=deny", etc.
	for _, sc := range faultableSyscalls {
		// Match "syscall=deny(" or "syscall=delay(" or "syscall=allow("
		if strings.Contains(src, sc+"=deny(") ||
			strings.Contains(src, sc+"=delay(") ||
			strings.Contains(src, sc+"=allow(") ||
			strings.Contains(src, sc+"=deny (") ||
			strings.Contains(src, sc+"=delay (") {
			found[sc] = true
		}
		// Match trace() syscall list: "write" or 'write' inside trace()/trace_start().
		if strings.Contains(src, "trace(") || strings.Contains(src, "trace_start(") {
			if strings.Contains(src, `"`+sc+`"`) || strings.Contains(src, `'`+sc+`'`) {
				found[sc] = true
			}
		}
	}

	// partition() always needs connect.
	if strings.Contains(src, "partition(") {
		found["connect"] = true
	}

	// Virtual time needs time syscalls.
	if rt.virtualTime {
		found["nanosleep"] = true
		found["clock_nanosleep"] = true
		found["clock_gettime"] = true
	}

	// Expand syscall families so the seccomp filter covers all variants.
	for _, sc := range []string{"write", "read", "open", "fsync", "sendto", "recvfrom"} {
		if found[sc] {
			for _, expanded := range expandSyscallFamily(sc) {
				found[expanded] = true
			}
		}
	}

	// Convert to sorted slice for deterministic filter.
	var syscalls []string
	for sc := range found {
		syscalls = append(syscalls, sc)
	}
	sort.Strings(syscalls)
	return syscalls
}

// serviceVarMap builds a mapping from Starlark variable names to service names.
// e.g., if the user wrote `pg = service("postgres", ...)`, returns {"pg": "postgres"}.
func (rt *Runtime) serviceVarMap() map[string]string {
	result := make(map[string]string)
	for varName, val := range rt.globals {
		if svc, ok := val.(*ServiceDef); ok {
			result[varName] = svc.Name
		}
	}
	return result
}

// requiredSyscallsForService returns the syscalls that need seccomp interception
// for a specific service. Scans the source for fault()/fault_start()/partition()
// calls that target this service.
// Returns nil if the service is never faulted (should use launchSimple).
func (rt *Runtime) requiredSyscallsForService(svcName string) []string {
	src := rt.sourceText
	varMap := rt.serviceVarMap()

	// Find all variable names that map to this service.
	var varNames []string
	for v, name := range varMap {
		if name == svcName {
			varNames = append(varNames, v)
		}
	}

	found := make(map[string]bool)

	// Scan line-by-line for fault/trace calls targeting this service.
	lines := strings.Split(src, "\n")
	for _, varName := range varNames {
		faultPrefix := "fault(" + varName + ","
		faultStartPrefix := "fault_start(" + varName + ","
		tracePrefix := "trace(" + varName + ","
		traceStartPrefix := "trace_start(" + varName + ","

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)

			// Fault calls: extract syscall keywords from fault definitions.
			if strings.Contains(trimmed, faultPrefix) || strings.Contains(trimmed, faultStartPrefix) {
				for _, sc := range faultableSyscalls {
					if strings.Contains(trimmed, sc+"=deny(") ||
						strings.Contains(trimmed, sc+"=delay(") ||
						strings.Contains(trimmed, sc+"=allow(") {
						found[sc] = true
					}
				}
			}

			// Trace calls: extract syscall names from syscalls=[...] list.
			if strings.Contains(trimmed, tracePrefix) || strings.Contains(trimmed, traceStartPrefix) {
				for _, sc := range faultableSyscalls {
					if strings.Contains(trimmed, `"`+sc+`"`) || strings.Contains(trimmed, `'`+sc+`'`) {
						found[sc] = true
					}
				}
			}
		}

		// partition(VAR_A, VAR_B, ...) needs connect for both services.
		for _, line := range lines {
			if strings.Contains(line, "partition(") &&
				(strings.Contains(line, varName+",") || strings.Contains(line, ", "+varName)) {
				found["connect"] = true
			}
		}
	}

	// Virtual time needs time syscalls on all services.
	if rt.virtualTime {
		found["nanosleep"] = true
		found["clock_nanosleep"] = true
		found["clock_gettime"] = true
	}

	// Expand syscall families.
	for _, sc := range []string{"write", "read", "open", "fsync", "sendto", "recvfrom"} {
		if found[sc] {
			for _, expanded := range expandSyscallFamily(sc) {
				found[expanded] = true
			}
		}
	}

	if len(found) == 0 {
		return nil
	}

	var syscalls []string
	for sc := range found {
		syscalls = append(syscalls, sc)
	}
	sort.Strings(syscalls)
	return syscalls
}

// requiredFaultRulesForService returns placeholder fault rules for a service.
func (rt *Runtime) requiredFaultRulesForService(svcName string) []engine.FaultRule {
	syscalls := rt.requiredSyscallsForService(svcName)
	if syscalls == nil {
		return nil
	}
	rules := make([]engine.FaultRule, len(syscalls))
	for i, sc := range syscalls {
		rules[i] = engine.FaultRule{
			Syscall:     sc,
			Action:      engine.ActionDeny,
			Probability: 0,
		}
	}
	return rules
}

// requiredFaultRules returns placeholder fault rules for syscalls referenced
// in the loaded Starlark source (global, all services). Used as fallback.
func (rt *Runtime) requiredFaultRules() []engine.FaultRule {
	syscalls := rt.requiredSyscalls()
	rules := make([]engine.FaultRule, len(syscalls))
	for i, sc := range syscalls {
		rules[i] = engine.FaultRule{
			Syscall:     sc,
			Action:      engine.ActionDeny,
			Probability: 0,
		}
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
		// Expand syscall families: "write" → write, writev, pwrite64
		syscalls := expandSyscallFamily(syscall)
		for _, sc := range syscalls {
			rule := engine.FaultRule{
				Syscall:     sc,
				Probability: fd.Probability,
			}
			switch fd.Action {
			case "delay":
				rule.Action = engine.ActionDelay
				rule.Delay = fd.Delay
			case "deny":
				rule.Action = engine.ActionDeny
				parsed, err := engine.ParseFaultRule(sc + "=" + fd.Errno + ":100%")
				if err == nil {
					rule.Errno = parsed.Errno
				}
			}
			rules = append(rules, rule)
		}
	}

	rs.session.SetDynamicFaultRules(rules)

	// Build fault summary for event log (helps diagnose "fault didn't fire").
	faultDetails := make(map[string]string)
	for syscall, fd := range faults {
		expanded := expandSyscallFamily(syscall)
		faultDetails[syscall] = fmt.Sprintf("%s(%s) → filter:[%s]", fd.Action, fd.Errno, strings.Join(expanded, ","))
	}
	rt.events.Emit("fault_applied", svcName, faultDetails)
	return nil
}

// applyTrace installs trace-only rules for a service's running session.
// Traced syscalls are allowed but logged at Info level.
func (rt *Runtime) applyTrace(svcName string, syscalls []string) error {
	rs, ok := rt.sessions[svcName]
	if !ok {
		return fmt.Errorf("service %q is not running", svcName)
	}

	var rules []engine.FaultRule
	for _, sc := range syscalls {
		for _, expanded := range expandSyscallFamily(sc) {
			rules = append(rules, engine.FaultRule{
				Syscall:     expanded,
				Action:      engine.ActionTrace,
				Probability: 1.0,
			})
		}
	}

	rs.session.SetDynamicFaultRules(rules)

	traceDetails := make(map[string]string)
	for _, sc := range syscalls {
		expanded := expandSyscallFamily(sc)
		traceDetails[sc] = fmt.Sprintf("trace → filter:[%s]", strings.Join(expanded, ","))
	}
	rt.events.Emit("trace_applied", svcName, traceDetails)
	return nil
}

// removeTrace clears trace rules for a service.
func (rt *Runtime) removeTrace(svcName string) {
	if rs, ok := rt.sessions[svcName]; ok {
		rs.session.ClearDynamicFaultRules()
	}
	rt.events.Emit("trace_removed", svcName, nil)
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
	port := ref.Interface.Port
	if ref.Interface.HostPort > 0 {
		port = ref.Interface.HostPort
	}
	addr := fmt.Sprintf("localhost:%d", port)
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
