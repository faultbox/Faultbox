package star

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	faultbox "github.com/faultbox/Faultbox"
	"github.com/faultbox/Faultbox/internal/config"
	"github.com/faultbox/Faultbox/internal/container"
	"github.com/faultbox/Faultbox/internal/engine"
	"github.com/faultbox/Faultbox/internal/eventsource"
	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/protocol"
	"github.com/faultbox/Faultbox/internal/proxy"
	"github.com/faultbox/Faultbox/internal/seccomp"
)

// TestResult captures the outcome of one test function.
type TestResult struct {
	Name        string         `json:"name"`
	Result      string         `json:"result"` // "pass" or "fail"
	Reason      string         `json:"reason,omitempty"`
	Seed        uint64         `json:"seed"`
	DurationMs  int64          `json:"duration_ms"`
	Events      []Event        `json:"events,omitempty"`
	ReturnValue starlark.Value `json:"-"`          // scenario return value for fault_scenario/fault_matrix
	Matrix      *MatrixInfo    `json:"-"`          // non-nil if from fault_matrix()
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
	loadedSpecs    map[string][]byte      // absolute path → bytes of every local .star loaded (RFC-025 Phase 4)
	rootSpec       string                 // absolute path of the root spec (LoadFile argument)

	// Seed for deterministic probabilistic faults (nil = random).
	seed *uint64

	// Virtual time: skip fault delays, advance virtual clock instead.
	virtualTime bool

	// Exploration mode: "all", "sample", or "".
	exploreMode    string
	explorePerm    int // current permutation index for explore mode
	exploreHeldN   int // number of held syscalls observed in last explore run

	// Services excluded from interleaving control in parallel().
	nondetServices map[string]bool

	// Registered scenarios — happy paths registered via scenario() builtin.
	// Used by the generator and also run as tests.
	scenarios []ScenarioRegistration

	// Registered fault assumptions — named fault configurations.
	faultAssumptions map[string]*FaultAssumptionDef

	// Registered fault scenarios — composed tests from fault_scenario().
	faultScenarios map[string]*FaultScenarioDef

	// Protocol-level proxy manager.
	proxyMgr *proxy.Manager

	// ServiceStdout is where service stdout is written (default: os.Stdout).
	// Set to os.Stderr when --format json to keep stdout clean for JSON.
	ServiceStdout io.Writer

	// Monitor errors — collected during test execution, checked after test.
	monitorMu     sync.Mutex
	monitorErrors []error

	// inTest is true when RunTest is executing a test function.
	// Used by monitor() to auto-register when called inside a test.
	inTest bool

	// currentFaultScenario is set by makeFaultScenarioRunner during execution
	// so RunTest can copy MatrixInfo to the TestResult.
	currentFaultScenario *FaultScenarioDef

	// mockTLSImpl is lazy-initialized the first time a tls=True mock service
	// starts. The whole runtime shares one CA so clients can trust every
	// mock by trusting a single bundle.
	mockTLSOnce sync.Once
	mockTLSImpl *mockTLS
	mockTLSErr  error
}

type runningSession struct {
	session *engine.Session
	cancel  context.CancelFunc
	done    chan *engine.Result
}

// New creates a new Starlark runtime.
func New(logger *slog.Logger) *Runtime {
	rt := &Runtime{
		log:           logging.WithComponent(logger, "starlark"),
		events:        NewEventLog(),
		eng:           engine.New(logger),
		services:      make(map[string]*ServiceDef),
		sessions:      make(map[string]*runningSession),
		faults:        make(map[string]map[string]*FaultDef),
		ServiceStdout: os.Stdout,
	}
	rt.proxyMgr = proxy.NewManager(func(evt proxy.ProxyEvent) {
		rt.events.Emit("proxy", evt.To, evt.Fields)
	})
	return rt
}

// LoadFile executes a .star file, populating the service registry and
// discovering test_* functions.
func (rt *Runtime) LoadFile(path string) error {
	// Store base directory for resolving relative paths (build=, volumes=).
	absPath, err := filepath.Abs(path)
	if err == nil {
		rt.baseDir = filepath.Dir(absPath)
		rt.rootSpec = absPath
	}

	// Read source for syscall scanning.
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	rt.sourceText = string(src)
	// Record the root spec so bundles can capture it under spec/.
	// Transitive load()s get captured inside makeLoadFunc. RFC-025 Phase 4.
	if rt.loadedSpecs == nil {
		rt.loadedSpecs = make(map[string][]byte)
	}
	if absPath != "" {
		rt.loadedSpecs[absPath] = append([]byte(nil), src...)
	}

	thread := &starlark.Thread{Name: "load"}
	thread.Load = rt.makeLoadFunc()

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
	thread.Load = rt.makeLoadFunc()
	rt.sourceText = src
	globals, err := starlark.ExecFile(thread, name, src, rt.builtins())
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	rt.globals = globals
	return nil
}

// stdlibPrefix is the module prefix that resolves to the embedded standard
// recipe library. See RFC-019 for the distribution convention.
const stdlibPrefix = "@faultbox/"

// makeLoadFunc creates a thread.Load handler for Starlark's load() statement.
// Loaded modules share the same runtime (service registry, builtins).
// Results are cached — each module is executed at most once.
//
// Resolution order:
//  1. "@faultbox/<path>" → embedded recipes FS (baked into the binary)
//  2. absolute path → read from filesystem
//  3. relative path → read from rt.baseDir (typically the spec's directory)
func (rt *Runtime) makeLoadFunc() func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	cache := make(map[string]starlark.StringDict)

	return func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
		// Return cached result if already loaded.
		if globals, ok := cache[module]; ok {
			return globals, nil
		}

		var src []byte
		var err error
		var modPath string

		if strings.HasPrefix(module, stdlibPrefix) {
			// Embedded stdlib lookup. The embed.FS keys drop the leading
			// "@faultbox/", so "@faultbox/recipes/mongodb.star" becomes
			// "recipes/mongodb.star" in the FS.
			embeddedPath := strings.TrimPrefix(module, stdlibPrefix)
			src, err = faultbox.Recipes.ReadFile(embeddedPath)
			if err != nil {
				return nil, fmt.Errorf("load %q: not found in faultbox stdlib (run 'faultbox recipes list' to see available recipes): %w", module, err)
			}
			modPath = module // preserve the @faultbox/... display name
		} else {
			// Resolve path relative to the base directory.
			modPath = module
			if rt.baseDir != "" && !filepath.IsAbs(module) {
				modPath = filepath.Join(rt.baseDir, module)
			}
			src, err = os.ReadFile(modPath)
			if err != nil {
				return nil, fmt.Errorf("load %q: %w", module, err)
			}
			// Capture the loaded file so bundles can include it under
			// spec/. Absolute path keys; bundle builder normalises to
			// baseDir-relative at emit time. RFC-025 Phase 4.
			if abs, absErr := filepath.Abs(modPath); absErr == nil {
				if rt.loadedSpecs == nil {
					rt.loadedSpecs = make(map[string][]byte)
				}
				rt.loadedSpecs[abs] = append([]byte(nil), src...)
			}
		}

		// Append module source for syscall scanning.
		rt.sourceText += "\n" + string(src)

		modThread := &starlark.Thread{Name: module}
		modThread.Load = rt.makeLoadFunc() // support nested loads

		globals, err := starlark.ExecFile(modThread, modPath, src, rt.builtins())
		if err != nil {
			return nil, fmt.Errorf("load %q: %w", module, err)
		}

		cache[module] = globals
		return globals, nil
	}
}

// DiscoverTests returns sorted test function names.
// Includes test_* globals and scenario()-registered functions (as test_<name>).
func (rt *Runtime) DiscoverTests() []string {
	seen := make(map[string]bool)
	var names []string
	for name, val := range rt.globals {
		if strings.HasPrefix(name, "test_") {
			if _, ok := val.(starlark.Callable); ok {
				names = append(names, name)
				seen[name] = true
			}
		}
	}
	// Add scenario-registered functions as test_<name>.
	for _, s := range rt.scenarios {
		testName := "test_" + s.Name
		if !seen[testName] {
			names = append(names, testName)
			seen[testName] = true
			// Register in globals so RunTest can find it.
			rt.globals[testName] = s.Fn
		}
	}
	// Add fault_scenario-registered tests as test_<name>.
	for _, fs := range rt.faultScenarios {
		testName := "test_" + fs.Name
		if !seen[testName] {
			names = append(names, testName)
			seen[testName] = true
			// Register a wrapper callable in globals so RunTest can find it.
			rt.globals[testName] = rt.makeFaultScenarioRunner(fs)
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

// ScenarioRegistration records a happy-path function registered via scenario().
type ScenarioRegistration struct {
	Name string             // function name (e.g., "order_flow")
	Fn   starlark.Callable  // the Starlark callable
}

// Scenarios returns all registered scenario functions.
func (rt *Runtime) Scenarios() []ScenarioRegistration {
	return rt.scenarios
}

// FaultScenarios returns all registered fault scenarios (including fault_matrix-generated ones).
func (rt *Runtime) FaultScenarios() map[string]*FaultScenarioDef {
	return rt.faultScenarios
}

// LoadedSpecs returns every local .star file that LoadFile or a
// transitive `load()` brought in, keyed by the file's bundle-friendly
// relative path (e.g. "faultbox.star" or "helpers/jwt.star"). The
// returned map is safe to inspect but callers should not mutate the
// byte slices.
//
// `@faultbox/...` stdlib loads are excluded — they're baked into the
// binary and don't need to travel with the bundle. Loads that resolve
// outside the root spec's directory (absolute paths, `../`) are
// preserved under a `_external/<basename>` prefix so they don't
// clobber tree entries. RFC-025 Phase 4.
func (rt *Runtime) LoadedSpecs() map[string][]byte {
	if len(rt.loadedSpecs) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(rt.loadedSpecs))
	for abs, data := range rt.loadedSpecs {
		key := rt.bundleSpecKey(abs)
		out[key] = data
	}
	return out
}

// bundleSpecKey normalises an absolute path into the relative name a
// bundle should store it under. Files within rt.baseDir keep their
// tree layout; files outside it land under `_external/<basename>`
// with a collision-safe suffix. Returns paths with forward slashes
// regardless of host OS — tar bundles are portable.
func (rt *Runtime) bundleSpecKey(absPath string) string {
	if rt.baseDir == "" {
		return filepath.Base(absPath)
	}
	rel, err := filepath.Rel(rt.baseDir, absPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		// Escape hatch for files outside the spec tree.
		return "_external/" + filepath.Base(absPath)
	}
	return filepath.ToSlash(rel)
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

	// Auto-calculate permutation count for --explore=all without --runs.
	autoExplore := cfg.ExploreMode == "all" && cfg.Runs <= 0

	for _, name := range tests {
		if cfg.Filter != "" && name != "test_"+cfg.Filter && name != cfg.Filter {
			continue
		}

		testRuns := runs
		for run := 0; run < testRuns; run++ {
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
			rt.exploreHeldN = 0

			runLabel := name
			if testRuns > 1 {
				runLabel = fmt.Sprintf("%s [seed=%d]", name, seed)
			}
			rt.log.Info("running test", slog.String("test", runLabel))

			tr := rt.RunTest(ctx, name)
			tr.Seed = seed

			// After first run with --explore=all (auto), calculate total permutations.
			if autoExplore && run == 0 && rt.exploreHeldN > 0 {
				testRuns = engine.Factorial(rt.exploreHeldN)
				rt.log.Info("explore=all: auto-calculated permutations",
					slog.Int("held_syscalls", rt.exploreHeldN),
					slog.Int("total_permutations", testRuns),
				)
			}

			if testRuns > 1 {
				rt.log.Info("test completed",
					slog.String("test", name),
					slog.String("result", tr.Result),
					slog.Uint64("seed", seed),
					slog.Int("run", run+1),
					slog.Int("of", testRuns),
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
				if testRuns > 1 {
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
// Tears down any reused containers that survived between tests.
func (rt *Runtime) cleanup() {
	// Tear down reused containers that were kept alive across tests.
	rt.stopReusedServices()

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

	// Start services — use a generous timeout covering image pull + container
	// startup + seccomp fd passing. Healthcheck waits are handled separately
	// with the spec-defined timeout so this budget is for infrastructure only.
	testCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
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
	thread := &starlark.Thread{
		Name: name,
		Print: func(_ *starlark.Thread, msg string) {
			fmt.Fprintln(os.Stderr, msg)
		},
	}
	rt.inTest = true
	retVal, err := starlark.Call(thread, fn, nil, nil)
	rt.inTest = false

	// Capture matrix info before clearing (set by makeFaultScenarioRunner).
	var matrixInfo *MatrixInfo
	if rt.currentFaultScenario != nil {
		matrixInfo = rt.currentFaultScenario.Matrix
	}

	// Stop services.
	rt.stopServices()

	events := rt.events.Events()

	if err != nil {
		return TestResult{
			Name: name, Result: "fail",
			Reason:     err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
			Events:     events,
			Matrix:     matrixInfo,
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
			Matrix:     matrixInfo,
		}
	}

	return TestResult{
		Name:        name,
		Result:      "pass",
		DurationMs:  time.Since(start).Milliseconds(),
		Events:      events,
		ReturnValue: retVal,
		Matrix:      matrixInfo,
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
// Services with reuse=True that already have a running session are skipped.
func (rt *Runtime) startServices(ctx context.Context) error {
	rt.mu.Lock()
	order := make([]string, len(rt.order))
	copy(order, rt.order)
	rt.mu.Unlock()

	// Clean up stale containers from previous tests or interrupted runs.
	// Only clean up if we don't have reused containers already running.
	if rt.dockerClient != nil && len(rt.sessions) == 0 {
		rt.dockerClient.CleanupStale(ctx)
	}

	for _, svcName := range order {
		svc := rt.services[svcName]

		// Skip services that are already running (reused from previous test).
		if _, running := rt.sessions[svcName]; running {
			rt.log.Info("reusing container from previous test",
				slog.String("service", svcName),
			)
			rt.events.Emit("service_started", svcName, nil)
			rt.events.Emit("service_ready", svcName, nil)

			// Run reset (or seed as fallback) to re-initialize state between tests.
			if err := rt.runResetCallback(svcName, svc); err != nil {
				return fmt.Errorf("reset service %q: %w", svcName, err)
			}
			continue
		}

		var err error
		switch {
		case svc.IsMock():
			err = rt.startMockService(ctx, svcName, svc)
		case svc.IsContainer():
			err = rt.startContainerService(ctx, svcName, svc)
		default:
			err = rt.startBinaryService(ctx, svcName, svc)
		}
		if err != nil {
			return err
		}

		// Run seed callback for newly started services.
		if svc.Seed != nil {
			if err := rt.runSeedCallback(svcName, svc); err != nil {
				return fmt.Errorf("seed service %q: %w", svcName, err)
			}
		}

		// RFC-024: pre-start a pass-through proxy for every proxy-capable
		// interface. With no rules installed the proxy just forwards bytes,
		// so the SUT sees identical behaviour to today. The upside is that
		// any later fault(iface_ref, rule) call dispatches rules against
		// the proxy the SUT is already talking to — closing the gap where
		// protocol-level faults were cosmetic because the app bypassed the
		// proxy. Mock services have no upstream and are skipped.
		if !svc.IsMock() {
			if err := rt.preStartProxies(ctx, svcName, svc); err != nil {
				rt.log.Warn("pre-start proxy failed (faults may bypass app traffic)",
					slog.String("service", svcName),
					slog.String("error", err.Error()),
				)
			}
		}
	}
	return nil
}

// preStartProxies spins up a pass-through proxy per supported interface on a
// just-started service. Subsequent services' env vars (built in buildEnv) will
// then resolve upstream-interface references to the proxy listen address so
// the SUT's own traffic reaches the proxy. Best-effort: a proxy failure is
// logged but does not block startup — tests that don't use protocol-level
// fault injection still work against the real upstream.
func (rt *Runtime) preStartProxies(ctx context.Context, svcName string, svc *ServiceDef) error {
	for ifaceName, iface := range svc.Interfaces {
		if !proxy.SupportsProxy(iface.Protocol) {
			continue
		}
		if rt.proxyMgr.GetProxyAddr(svcName, ifaceName) != "" {
			continue // already running (container reuse across tests)
		}
		port := iface.Port
		if iface.HostPort > 0 {
			port = iface.HostPort
		}
		targetAddr := fmt.Sprintf("127.0.0.1:%d", port)
		proxyAddr, err := rt.proxyMgr.EnsureProxy(ctx, svcName, ifaceName, iface.Protocol, targetAddr)
		if err != nil {
			return fmt.Errorf("proxy %s.%s: %w", svcName, ifaceName, err)
		}
		rt.events.Emit("proxy_started", svcName, map[string]string{
			"interface": ifaceName,
			"protocol":  iface.Protocol,
			"listen":    proxyAddr,
			"target":    targetAddr,
			"mode":      "passthrough",
		})
	}
	return nil
}

// runSeedCallback executes the seed() Starlark callable for a service.
// Called once after first healthcheck to initialize service state.
func (rt *Runtime) runSeedCallback(svcName string, svc *ServiceDef) error {
	rt.log.Info("running seed", slog.String("service", svcName))
	rt.events.Emit("service_seed", svcName, nil)

	thread := &starlark.Thread{
		Name: fmt.Sprintf("seed:%s", svcName),
		Print: func(_ *starlark.Thread, msg string) {
			fmt.Fprintln(os.Stderr, msg)
		},
	}
	_, err := starlark.Call(thread, svc.Seed, nil, nil)
	if err != nil {
		return fmt.Errorf("seed() failed: %w", err)
	}
	rt.log.Info("seed complete", slog.String("service", svcName))
	return nil
}

// runResetCallback executes the reset() (or seed() as fallback) Starlark
// callable for a reused service. Called before each test except the first.
func (rt *Runtime) runResetCallback(svcName string, svc *ServiceDef) error {
	cb := svc.Reset
	label := "reset"
	if cb == nil {
		cb = svc.Seed
		label = "seed (as reset)"
	}
	if cb == nil {
		return nil // no lifecycle callback — nothing to do
	}

	rt.log.Info("running "+label, slog.String("service", svcName))
	rt.events.Emit("service_reset", svcName, nil)

	thread := &starlark.Thread{
		Name: fmt.Sprintf("reset:%s", svcName),
		Print: func(_ *starlark.Thread, msg string) {
			fmt.Fprintln(os.Stderr, msg)
		},
	}
	_, err := starlark.Call(thread, cb, nil, nil)
	if err != nil {
		return fmt.Errorf("%s() failed: %w", label, err)
	}
	rt.log.Info(label+" complete", slog.String("service", svcName))
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

	// Set up stdout: if observe includes stdout source, create an OS pipe
	// so the child process writes directly to it. We tee from the read-end
	// to both the decoder and the configured output.
	svcStdout := rt.ServiceStdout
	var stdoutFile *os.File // if set, overrides child's fd 1
	var stdoutSources []*eventsource.StdoutSourceHandle

	for _, obs := range svc.Observe {
		if obs.SourceName == "stdout" {
			// Create a real OS pipe — the child writes to pipeW (fd),
			// we read from pipeR in a goroutine.
			pipeR, pipeW, err := os.Pipe()
			if err != nil {
				return fmt.Errorf("os.Pipe for stdout observe: %w", err)
			}
			stdoutFile = pipeW

			// Create and start the stdout event source.
			var dec eventsource.Decoder
			if obs.DecoderName != "" {
				if factory, ok := eventsource.GetDecoder(obs.DecoderName); ok {
					d, err := factory(obs.Params)
					if err != nil {
						pipeR.Close()
						pipeW.Close()
						return fmt.Errorf("decoder %q: %w", obs.DecoderName, err)
					}
					dec = d
				}
			}

			// Tee: copy pipe output to both the decoder and the console.
			decoderPR, decoderPW := io.Pipe()
			go func() {
				defer decoderPW.Close()
				defer pipeR.Close()
				io.Copy(io.MultiWriter(svcStdout, decoderPW), pipeR)
			}()

			src := eventsource.StdoutSource(dec)
			src.StartWithReader(ctx, eventsource.SourceConfig{
				ServiceName: svcName,
				Emit: func(typ string, fields map[string]string) {
					rt.events.Emit(typ, svcName, fields)
				},
			}, decoderPR)
			stdoutSources = append(stdoutSources, &eventsource.StdoutSourceHandle{
				Source:    src,
				PipeWrite: decoderPW,
			})
		}
	}

	// Build session stdout: use the OS pipe file if observe is active,
	// otherwise use the configured writer.
	var sessStdout io.Writer = svcStdout
	if stdoutFile != nil {
		sessStdout = stdoutFile
	}

	sessCfg := engine.SessionConfig{
		Binary:             svc.Binary,
		Args:               svc.Args,
		Env:                envVars,
		Stdout:             sessStdout,
		Stderr:             rt.ServiceStdout,
		Namespaces:         ns,
		FaultRules:         faultRules,
		OnSyscall:          onSyscall,
		Seed:               rt.seed,
		VirtualTime:        rt.virtualTime,
		ExternalListenerFd: -1, // not external
	}

	_ = stdoutSources // TODO: store for cleanup in stopServices
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

	// Resolve ports from interfaces, honouring any explicit host port overrides.
	ports := make(map[int]int)
	for _, iface := range svc.Interfaces {
		if hp, ok := svc.Ports[iface.Port]; ok {
			ports[iface.Port] = hp // explicit host port (may be 0 = Docker picks)
		} else {
			ports[iface.Port] = 0 // default: let Docker pick host port
		}
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
		NoSeccomp:  svc.NoSeccomp,   // honor `seccomp = False` opt-out
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

	// Warn if fault rules target this service but seccomp is not active.
	if result.ListenerFd < 0 && len(svcRules) > 0 {
		rt.log.Warn("fault rules will NOT apply — service running without seccomp",
			slog.String("service", svcName),
			slog.Int("fault_rules", len(svcRules)),
			slog.String("hint", "multi-process entrypoint prevented seccomp; faults on this service are silently skipped"),
		)
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
			hcCtx, hcCancel := context.WithTimeout(context.Background(), timeout)
			defer hcCancel()
			if err := waitReady(hcCtx, hcTest, timeout); err != nil {
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
		if evt.Label != "" {
			fields["label"] = evt.Label
		}
		if evt.Op != "" {
			fields["op"] = evt.Op
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

	// Wait for healthcheck using a fresh context derived from the background —
	// not from startServices' testCtx (which has a short 30s deadline).
	// The healthcheck defines its own maximum wait time via the timeout parameter.
	if svc.Healthcheck != nil {
		timeout := svc.Healthcheck.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		// For containers with mapped ports, adjust the healthcheck URL.
		hcTest := rt.resolveHealthcheck(svc)
		hcCtx, hcCancel := context.WithTimeout(context.Background(), timeout)
		defer hcCancel()
		if err := waitReady(hcCtx, hcTest, timeout); err != nil {
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
	// Auto-inject FAULTBOX_* for all registered services. For container
	// consumers, a running proxy on the host is reachable at
	// `host.docker.internal:<proxy_port>` (added via ExtraHosts on
	// container create) rather than the binary-mode `127.0.0.1:<port>`.
	// Host-mode fallback when no proxy is running: keep the previous
	// behaviour (service name DNS for container targets, localhost for
	// binary targets).
	for _, otherName := range rt.order {
		other := rt.services[otherName]
		for _, ifName := range sortedInterfaceNames(other) {
			iface := other.Interfaces[ifName]
			prefix := fmt.Sprintf("FAULTBOX_%s_%s", strings.ToUpper(otherName), strings.ToUpper(ifName))

			host, port := "", iface.Port
			if proxyAddr := rt.proxyMgr.GetProxyAddr(otherName, ifName); proxyAddr != "" {
				if _, pp, err := splitHostPort(proxyAddr); err == nil {
					host, port = "host.docker.internal", pp
				}
			}
			if host == "" {
				if other.IsContainer() {
					host = otherName
				} else {
					host = "localhost"
				}
			}
			env = append(env,
				fmt.Sprintf("%s_HOST=%s", prefix, host),
				fmt.Sprintf("%s_PORT=%d", prefix, port),
				fmt.Sprintf("%s_ADDR=%s:%d", prefix, host, port),
			)
		}
	}
	// User-defined env, with proxy-aware substring substitution so values
	// that contain real upstream addrs (e.g. "postgres://u:p@db:5432/…")
	// get redirected to the proxy via host.docker.internal. Mirrors the
	// binary-mode behaviour in buildEnv; shipped in v0.9.6 as RFC-024
	// container follow-up.
	subs := rt.proxyAddrSubstitutionsFor(containerConsumer)
	for k, v := range svc.Env {
		env = append(env, k+"="+applyAddrSubstitutions(v, subs))
	}
	return env
}

// sortedInterfaceNames returns a service's interface keys in deterministic
// order. Without this, FAULTBOX_* env ordering changes between processes,
// which makes tests that assert env contents flaky. Cheap on any realistic
// interface count.
func sortedInterfaceNames(svc *ServiceDef) []string {
	out := make([]string, 0, len(svc.Interfaces))
	for n := range svc.Interfaces {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
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
// Services with Reuse=true are kept alive — only their dynamic fault rules
// are cleared. They are torn down later by stopReusedServices().
func (rt *Runtime) stopServices() {
	// Determine which services are reused.
	reused := make(map[string]bool)
	for name := range rt.sessions {
		if svc, ok := rt.services[name]; ok && svc.Reuse {
			reused[name] = true
		}
	}

	// Cancel non-reused sessions and tear down their proxies. Leaving
	// the proxy alive across tests would cause the next test's
	// EnsureProxy call to reuse a listener whose target points at the
	// now-dead PID, so real traffic stalls until the proxy read
	// timeout fires (see #61).
	for name, rs := range rt.sessions {
		if reused[name] {
			// Keep session alive; just clear dynamic fault rules.
			rs.session.ClearDynamicFaultRules()
			continue
		}
		rs.cancel()
		if rt.proxyMgr != nil {
			rt.proxyMgr.StopService(name)
		}
	}
	// Wait for non-reused sessions to finish.
	kept := make(map[string]*runningSession)
	for name, rs := range rt.sessions {
		if reused[name] {
			kept[name] = rs
			continue
		}
		select {
		case <-rs.done:
		case <-time.After(5 * time.Second):
		}
	}
	rt.sessions = kept

	// Stop and remove non-reused Docker containers.
	if rt.dockerClient != nil {
		ctx := context.Background()
		keptContainers := make(map[string]string)
		for name, cid := range rt.containerIDs {
			if reused[name] {
				keptContainers[name] = cid
				continue
			}
			rt.log.Debug("stopping container", slog.String("name", name))
			rt.dockerClient.StopContainer(ctx, cid, 5)
			rt.dockerClient.RemoveContainer(ctx, cid)
		}
		rt.containerIDs = keptContainers

		// Remove the Docker network only if no reused containers remain.
		if len(keptContainers) == 0 && rt.networkID != "" {
			rt.dockerClient.RemoveNetwork(ctx, rt.networkID)
			rt.networkID = ""
		}
	}

	// Clean up socket directories only for non-reused services.
	if len(reused) == 0 {
		socketBase := filepath.Join(os.TempDir(), "faultbox-sockets")
		os.RemoveAll(socketBase)
	}

	// Clear active faults.
	rt.faultsMu.Lock()
	rt.faults = make(map[string]map[string]*FaultDef)
	rt.faultsMu.Unlock()
}

// stopReusedServices tears down containers that were kept alive via reuse=True.
// Called once at suite end by cleanup().
func (rt *Runtime) stopReusedServices() {
	for name, rs := range rt.sessions {
		rt.log.Debug("stopping reused session", slog.String("service", name))
		rs.cancel()
	}
	for _, rs := range rt.sessions {
		select {
		case <-rs.done:
		case <-time.After(5 * time.Second):
		}
	}
	rt.sessions = make(map[string]*runningSession)

	if rt.dockerClient != nil {
		ctx := context.Background()
		for name, cid := range rt.containerIDs {
			rt.log.Debug("removing reused container", slog.String("name", name))
			rt.dockerClient.StopContainer(ctx, cid, 5)
			rt.dockerClient.RemoveContainer(ctx, cid)
		}
		rt.containerIDs = make(map[string]string)
	}

	socketBase := filepath.Join(os.TempDir(), "faultbox-sockets")
	os.RemoveAll(socketBase)
}

// buildEnv creates the environment for a service with auto-injection.
//
// RFC-024: if a proxy has been pre-started for an interface, the injected
// FAULTBOX_<SVC>_<IFACE>_{HOST,PORT,ADDR} vars point at the proxy rather
// than the real upstream. This puts the proxy in the SUT's data path so
// protocol-level fault rules (response/error/drop) actually fire against
// app-initiated traffic. When no proxy exists (e.g., tcp, mock services),
// the vars fall back to the real upstream addr — same as v0.9.4 behaviour.
func (rt *Runtime) buildEnv(svc *ServiceDef) []string {
	env := make(map[string]string)

	// Auto-inject FAULTBOX_* vars for all services.
	for name, s := range rt.services {
		upper := strings.ToUpper(name)
		for ifName, iface := range s.Interfaces {
			ifUpper := strings.ToUpper(ifName)
			prefix := fmt.Sprintf("FAULTBOX_%s_%s", upper, ifUpper)

			host, port := "localhost", iface.Port
			if proxyAddr := rt.proxyMgr.GetProxyAddr(name, ifName); proxyAddr != "" {
				if ph, pp, err := splitHostPort(proxyAddr); err == nil {
					host, port = ph, pp
				}
			}
			env[prefix+"_HOST"] = host
			env[prefix+"_PORT"] = fmt.Sprintf("%d", port)
			env[prefix+"_ADDR"] = fmt.Sprintf("%s:%d", host, port)
		}
	}

	// User-defined env. At spec-load time, .addr / .internal_addr returned
	// the real upstream address (no proxy existed yet), so values look like
	// "localhost:5432/mydb". Now that proxies are pre-started (RFC-024),
	// rewrite those substrings so app-initiated traffic goes via the proxy.
	// Substitution table: one entry per (upstream real addr → proxy addr)
	// pair across every interface that has a running proxy.
	substitutions := rt.proxyAddrSubstitutions()
	for k, v := range svc.Env {
		env[k] = applyAddrSubstitutions(v, substitutions)
	}

	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}

// consumerMode identifies whether the SUT receiving a substituted address
// runs on the host (binary) or inside a Docker container. Container
// consumers can't dial the host-side proxy via 127.0.0.1, so the
// substitution target becomes host.docker.internal — which Faultbox wires
// up at container create time via ExtraHosts.
type consumerMode int

const (
	binaryConsumer consumerMode = iota
	containerConsumer
)

// proxyAddrSubstitutions builds a map of "upstream real addr" → "proxy
// addr" for every interface that has a running proxy. Retained for the
// default (binary) caller; see proxyAddrSubstitutionsFor for the
// mode-aware variant that container env building needs.
func (rt *Runtime) proxyAddrSubstitutions() map[string]string {
	return rt.proxyAddrSubstitutionsFor(binaryConsumer)
}

// proxyAddrSubstitutionsFor builds the rewrite table targeting either
// binary or container consumers. For container consumers, proxy addrs
// are re-spelled with host.docker.internal so the SUT, isolated in its
// network namespace, can still reach the host-side listener.
func (rt *Runtime) proxyAddrSubstitutionsFor(mode consumerMode) map[string]string {
	out := make(map[string]string)
	for name, s := range rt.services {
		for ifName, iface := range s.Interfaces {
			proxyAddr := rt.proxyMgr.GetProxyAddr(name, ifName)
			if proxyAddr == "" {
				continue
			}
			target := proxyAddr
			if mode == containerConsumer {
				if _, pp, err := splitHostPort(proxyAddr); err == nil {
					target = fmt.Sprintf("host.docker.internal:%d", pp)
				}
			}
			out[fmt.Sprintf("localhost:%d", iface.Port)] = target
			out[fmt.Sprintf("127.0.0.1:%d", iface.Port)] = target
			out[fmt.Sprintf("%s:%d", name, iface.Port)] = target
		}
	}
	return out
}

// applyAddrSubstitutions rewrites any real-upstream-addr substring in v to
// the corresponding proxy addr. No-op when subs is empty.
func applyAddrSubstitutions(v string, subs map[string]string) string {
	if len(subs) == 0 {
		return v
	}
	for real, proxy := range subs {
		v = strings.ReplaceAll(v, real, proxy)
	}
	return v
}

// splitHostPort parses "host:port" into its parts with port as int. Wraps
// net.SplitHostPort so buildEnv has a single integer port to inject into
// FAULTBOX_*_PORT env vars.
func splitHostPort(addr string) (string, int, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("parse port %q: %w", p, err)
	}
	return h, port, nil
}

// makeFaultScenarioRunner creates a Starlark callable that, when invoked by
// RunTest, executes the fault_scenario lifecycle: apply faults → register
// monitors → run scenario → call expect → cleanup.
func (rt *Runtime) makeFaultScenarioRunner(fs *FaultScenarioDef) starlark.Callable {
	return starlark.NewBuiltin("test_"+fs.Name, func(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		// Track current fault_scenario so RunTest can copy MatrixInfo.
		rt.currentFaultScenario = fs
		defer func() { rt.currentFaultScenario = nil }()

		// 1. Collect and register all monitors (from assumptions + scenario-level).
		var monitorIDs []int
		for _, fa := range fs.Faults {
			for _, m := range fa.Monitors {
				monitorIDs = append(monitorIDs, rt.RegisterMonitor(m))
			}
		}
		for _, m := range fs.Monitors {
			monitorIDs = append(monitorIDs, rt.RegisterMonitor(m))
		}
		defer func() {
			for _, id := range monitorIDs {
				rt.UnregisterMonitor(id)
			}
		}()

		// 2. Apply fault rules from all assumptions.
		appliedServices := make(map[string]bool)
		for _, fa := range fs.Faults {
			// Group rules by target service.
			faultsByService := make(map[string]map[string]*FaultDef)
			for _, r := range fa.Rules {
				svcName := r.Target.Name
				if faultsByService[svcName] == nil {
					faultsByService[svcName] = make(map[string]*FaultDef)
				}
				faultsByService[svcName][r.Syscall] = r.Fault
			}
			for svcName, faults := range faultsByService {
				if err := rt.applyFaults(svcName, faults); err != nil {
					return nil, fmt.Errorf("fault_scenario %q: %w", fs.Name, err)
				}
				appliedServices[svcName] = true
			}
		}
		defer func() {
			for svcName := range appliedServices {
				rt.removeFaults(svcName)
			}
		}()

		// 2b. Apply protocol-level fault rules from all assumptions.
		// Without this, fault_assumption(rules=[...]) inside a
		// fault_scenario or fault_matrix is silently cosmetic — matching
		// the bug the fault(assumption, run=) path had before v0.9.4.
		type proxyKey struct{ svc, iface string }
		appliedProxies := make(map[proxyKey]struct{})
		for _, fa := range fs.Faults {
			for _, pr := range fa.ProxyRules {
				if pr.Target == nil || pr.Target.Service == nil || pr.ProxyFault == nil {
					continue
				}
				svcName := pr.Target.Service.Name
				ifaceName := pr.Target.Interface.Name
				proto := pr.Target.Interface.Protocol
				port := pr.Target.Interface.Port
				if pr.Target.Interface.HostPort > 0 {
					port = pr.Target.Interface.HostPort
				}
				targetAddr := fmt.Sprintf("127.0.0.1:%d", port)
				if _, err := rt.proxyMgr.EnsureProxy(context.Background(), svcName, ifaceName, proto, targetAddr); err != nil {
					return nil, fmt.Errorf("fault_scenario %q: proxy start for %s.%s: %w", fs.Name, svcName, ifaceName, err)
				}
				rt.proxyMgr.AddRule(svcName, ifaceName, proxyFaultToRule(pr.ProxyFault))
				appliedProxies[proxyKey{svcName, ifaceName}] = struct{}{}
				rt.events.Emit("proxy_fault_applied", svcName, map[string]string{
					"interface":  ifaceName,
					"protocol":   proto,
					"assumption": fa.Name,
				})
			}
		}
		defer func() {
			for k := range appliedProxies {
				rt.proxyMgr.ClearRules(k.svc, k.iface)
				rt.events.Emit("proxy_fault_removed", k.svc, map[string]string{
					"interface": k.iface,
				})
			}
		}()

		// 3. Run the scenario function and capture the return value.
		result, err := starlark.Call(thread, fs.Scenario, nil, nil)
		if err != nil {
			return nil, err
		}

		// 4. Check monitor errors before calling expect.
		rt.monitorMu.Lock()
		monErrs := rt.monitorErrors
		rt.monitorMu.Unlock()
		if len(monErrs) > 0 {
			return nil, fmt.Errorf("monitor violation: %v", monErrs[0])
		}

		// 5. Call expect(result) if provided.
		if fs.Expect != nil {
			if result == nil {
				result = starlark.None
			}
			_, err := starlark.Call(thread, fs.Expect, starlark.Tuple{result}, nil)
			if err != nil {
				return nil, err
			}
		}

		return result, nil
	})
}

// RegisterMonitor subscribes a MonitorDef to the event log.
// Returns the subscription ID for later unregistration.
func (rt *Runtime) RegisterMonitor(m *MonitorDef) int {
	// Convert EventFilter to eventFilter for the event log.
	filters := make([]eventFilter, len(m.Filters))
	for i, f := range m.Filters {
		filters[i] = eventFilter{key: f.Key, value: f.Value}
	}

	callback := m.Callback
	return rt.events.Subscribe(filters, func(ev Event) error {
		d := starlark.NewDict(6)
		d.SetKey(starlark.String("seq"), starlark.MakeInt64(ev.Seq))
		d.SetKey(starlark.String("type"), starlark.String(ev.Type))
		d.SetKey(starlark.String("service"), starlark.String(ev.Service))
		for k, v := range ev.Fields {
			d.SetKey(starlark.String(k), starlark.String(v))
		}

		t := &starlark.Thread{Name: "monitor"}
		_, err := starlark.Call(t, callback, starlark.Tuple{d}, nil)
		if err != nil {
			rt.monitorMu.Lock()
			rt.monitorErrors = append(rt.monitorErrors, err)
			rt.monitorMu.Unlock()
			return err
		}
		return nil
	})
}

// UnregisterMonitor removes a monitor subscription by ID.
func (rt *Runtime) UnregisterMonitor(id int) {
	rt.events.Unsubscribe(id)
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
	// Check globals (main file variables).
	for varName, val := range rt.globals {
		if svc, ok := val.(*ServiceDef); ok {
			result[varName] = svc.Name
		}
	}
	// Also map service names to themselves — covers load() imports
	// where the variable name matches the service name, and covers
	// cases where the variable isn't in globals (loaded via load()).
	for name := range rt.services {
		if _, exists := result[name]; !exists {
			result[name] = name
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
				// Named operations: check if any op name appears as a fault keyword.
				if svc, ok := rt.services[svcName]; ok && svc.Ops != nil {
					for opName, opDef := range svc.Ops {
						if strings.Contains(trimmed, opName+"=deny(") ||
							strings.Contains(trimmed, opName+"=delay(") ||
							strings.Contains(trimmed, opName+"=allow(") {
							for _, sc := range opDef.Syscalls {
								found[sc] = true
							}
						}
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

	// Union syscalls from any fault_assumption() targeting this service.
	// Without this, fault_assumption(target=svc, custom_op=deny(...)) inside
	// fault_scenario/fault_matrix would load correctly but the filter would
	// miss the underlying syscalls (custom ops referenced inside a
	// fault_assumption call are invisible to the text-pattern scanner above).
	// Customer-reported on v0.8.8 — fix shipped in v0.9.4.
	for _, a := range rt.faultAssumptions {
		for _, r := range a.Rules {
			if r.Target != nil && r.Target.Name == svcName && r.Syscall != "" {
				found[r.Syscall] = true
			}
		}
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
				Label:       fd.Label,
				Op:          fd.Op,
				PathGlob:    fd.PathGlob,
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
		summary := fmt.Sprintf("%s(%s) → filter:[%s]", fd.Action, fd.Errno, strings.Join(expanded, ","))
		if fd.Label != "" {
			summary += fmt.Sprintf(" label=%q", fd.Label)
		}
		faultDetails[syscall] = summary
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

// removeFaults clears fault rules for a service. Before clearing, it
// inspects per-rule match counters and emits a `fault_zero_traffic` event
// for any rule that never matched during the window — a common signal
// that the test driver didn't force fresh upstream traffic while the fault
// was active (e.g., the client cached an init-time response and reused it
// instead of re-calling the upstream). Customer-reported signal in v0.8.8;
// surfaced as an explicit event in v0.9.4 so users don't silently get
// "test passed" when no injection actually fired.
func (rt *Runtime) removeFaults(svcName string) {
	rt.faultsMu.Lock()
	delete(rt.faults, svcName)
	rt.faultsMu.Unlock()

	if rs, ok := rt.sessions[svcName]; ok {
		for _, r := range rs.session.DynamicRuleActivity() {
			if r.MatchCount > 0 {
				continue
			}
			fields := map[string]string{
				"syscall": r.Syscall,
				"action":  r.Action,
			}
			if r.Op != "" {
				fields["op"] = r.Op
			}
			if r.Label != "" {
				fields["label"] = r.Label
			}
			rt.events.Emit("fault_zero_traffic", svcName, fields)
		}
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

	// If a proxy is running for this interface, route through it.
	if proxyAddr := rt.proxyMgr.GetProxyAddr(targetSvc, ref.Interface.Name); proxyAddr != "" {
		addr = proxyAddr
	}

	// Merge test driver's clock into target service — records causal "request sent".
	rt.events.MergeClock(targetSvc, "test")

	// Emit step_send event from test driver — shows request going out.
	rt.events.Emit("step_send", "test", map[string]string{
		"target":    targetSvc,
		"method":    method,
		"interface": ref.Interface.Name,
		"protocol":  ref.Interface.Protocol,
	})

	// Dispatch via protocol plugin registry.
	p, ok := protocol.Get(ref.Interface.Protocol)
	if !ok {
		return nil, fmt.Errorf("unsupported protocol %q (no plugin registered)", ref.Interface.Protocol)
	}

	// Convert Starlark kwargs to map[string]any for the protocol plugin.
	stepArgs := starlarkKwargsToMap(kwargs)

	stepResult, err := p.ExecuteStep(context.Background(), addr, method, stepArgs)
	if err != nil {
		return nil, err
	}

	// After a step completes, merge target service's clock into the test driver.
	rt.events.MergeClock("test", targetSvc)

	// Emit step_recv event — shows response received, with merged clock.
	rt.events.Emit("step_recv", "test", map[string]string{
		"target": targetSvc,
		"method": method,
	})

	// TCP send returns raw string for backward compatibility.
	if ref.Interface.Protocol == "tcp" && stepResult.Success {
		return starlark.String(stepResult.Body), nil
	}
	if ref.Interface.Protocol == "tcp" && !stepResult.Success {
		return nil, fmt.Errorf("tcp send failed: %s", stepResult.Error)
	}

	return &Response{
		Status:     stepResult.StatusCode,
		Body:       stepResult.Body,
		DurationMs: stepResult.DurationMs,
		Ok:         stepResult.Success,
		Error:      stepResult.Error,
	}, nil
}

// starlarkKwargsToMap converts Starlark kwargs to a Go map for protocol plugins.
func starlarkKwargsToMap(kwargs []starlark.Tuple) map[string]any {
	m := make(map[string]any)
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch v := kv[1].(type) {
		case starlark.String:
			m[key] = string(v)
		case starlark.Int:
			n, _ := v.Int64()
			m[key] = n
		case starlark.Float:
			m[key] = float64(v)
		case starlark.Bool:
			m[key] = bool(v)
		case *starlark.Dict:
			dm := make(map[string]any)
			for _, item := range v.Items() {
				k, _ := starlark.AsString(item[0])
				val, _ := starlark.AsString(item[1])
				dm[k] = val
			}
			m[key] = dm
		default:
			m[key] = v.String()
		}
	}
	return m
}

// waitReady polls a readiness check using protocol plugins.
func waitReady(ctx context.Context, check string, timeout time.Duration) error {
	// Determine protocol from URL prefix.
	switch {
	case strings.HasPrefix(check, "tcp://"):
		addr := strings.TrimPrefix(check, "tcp://")
		if p, ok := protocol.Get("tcp"); ok {
			return p.Healthcheck(ctx, addr, timeout)
		}
		return protocol.TCPHealthcheck(ctx, addr, timeout)
	case strings.HasPrefix(check, "kafka://"):
		addr := strings.TrimPrefix(check, "kafka://")
		if p, ok := protocol.Get("kafka"); ok {
			return p.Healthcheck(ctx, addr, timeout)
		}
		return fmt.Errorf("kafka protocol not registered")
	case strings.HasPrefix(check, "http://"), strings.HasPrefix(check, "https://"):
		if p, ok := protocol.Get("http"); ok {
			return p.Healthcheck(ctx, check, timeout)
		}
		return fmt.Errorf("HTTP protocol not registered")
	default:
		return fmt.Errorf("unsupported healthcheck scheme in %q", check)
	}
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
