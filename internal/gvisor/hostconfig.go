package gvisor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Host registration for filesystem observation (RFC-056 M1).
//
// runsc installs a trace session at sandbox boot via --pod-init-config, which
// is a RUNTIME-level flag in daemon.json rather than a per-container option.
// So watch() needs one entry in the user's Docker daemon configuration, and
// that entry is host-wide state.
//
// Two rules follow, and both are load-bearing:
//
//  1. Nothing here runs implicitly. A test run that needs the registration
//     errors and names this command; it never edits a user's daemon config on
//     their behalf. Editing daemon.json is not a side effect a test should have.
//
//  2. Every change is reported before the user is asked to act on it. The
//     restart that applies the change stops every container on the machine, so
//     "what did you just do to my host" has to be answerable without reading
//     the file.

const (
	// TraceRuntimeName is the Docker runtime Faultbox registers. Distinct from
	// RuntimeName ("runsc") so an existing gVisor setup is never modified —
	// users who already run runsc for their own reasons keep it untouched.
	TraceRuntimeName = "faultbox-trace"

	// DaemonJSONPath is Docker's daemon configuration.
	DaemonJSONPath = "/etc/docker/daemon.json"

	// TraceConfigPath holds the pod-init trace session.
	TraceConfigPath = "/etc/faultbox/trace.json"

	// DefaultSinkPath is where the per-run sink binds. Fixed, because the
	// daemon config is written once and cannot know a future run's paths.
	DefaultSinkPath = "/run/faultbox/seccheck.sock"
)

// PointSet names a group of trace points that can be enabled together.
type PointSet string

const (
	// PointSetDefault is always installed: opens and the write family.
	PointSetDefault PointSet = "default"
	// PointSetRead adds read/pread64. Off by default — measured at roughly
	// double the traffic and 1,488 dropped points on a read-heavy workload,
	// where the same workload with the default set dropped none. Since a
	// dropped point makes an audit's "never" unprovable, drops fail the test;
	// enabling reads therefore trades observability for a real risk of an
	// unrunnable spec. See docs/implementation/v0.16.0-rfc-056-plan.md §0c.
	PointSetRead PointSet = "read"
	// PointSetClose adds close, for "opened and never closed" assertions.
	PointSetClose PointSet = "close"
	// PointSetConnect adds connect.
	PointSetConnect PointSet = "connect"
)

// pointsFor maps a set to its trace points.
func pointsFor(s PointSet) []TracePoint {
	common := []string{"time", "thread_id", "container_id", "process_name"}
	switch s {
	case PointSetDefault:
		return []TracePoint{
			{Name: "syscall/openat/exit", OptionalFields: []string{"fd_path"}, ContextFields: append([]string{"cwd"}, common...)},
			{Name: "syscall/write/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
			{Name: "syscall/pwrite64/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
			{Name: "syscall/writev/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		}
	case PointSetRead:
		return []TracePoint{
			{Name: "syscall/read/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
			{Name: "syscall/pread64/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		}
	case PointSetClose:
		return []TracePoint{{Name: "syscall/close/exit", OptionalFields: []string{"fd_path"}, ContextFields: common}}
	case PointSetConnect:
		return []TracePoint{{Name: "syscall/connect/exit", OptionalFields: []string{"fd_path"}, ContextFields: common}}
	}
	return nil
}

// SetupOptions configures what setup-trace installs.
type SetupOptions struct {
	// Extra point sets beyond the default.
	Extra []PointSet
	// SinkPath overrides DefaultSinkPath.
	SinkPath string
	// RunscPath overrides the discovered runsc binary.
	RunscPath string
	// DaemonJSON / TraceConfig override the file locations, for tests.
	DaemonJSON  string
	TraceConfig string
}

func (o SetupOptions) sinkPath() string {
	if o.SinkPath != "" {
		return o.SinkPath
	}
	return DefaultSinkPath
}

func (o SetupOptions) daemonJSON() string {
	if o.DaemonJSON != "" {
		return o.DaemonJSON
	}
	return DaemonJSONPath
}

func (o SetupOptions) traceConfig() string {
	if o.TraceConfig != "" {
		return o.TraceConfig
	}
	return TraceConfigPath
}

// Points returns every trace point the options ask for, default set first.
func (o SetupOptions) Points() []TracePoint {
	out := pointsFor(PointSetDefault)
	seen := map[PointSet]bool{PointSetDefault: true}
	for _, s := range o.Extra {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, pointsFor(s)...)
	}
	return out
}

// podInitConfig is runsc's --pod-init-config file.
type podInitConfig struct {
	TraceSession traceConfig `json:"trace_session"`
}

// Change is one modification setup-trace would make or did make.
//
// Held as data rather than printed as it happens so --check can report the
// same thing without writing, and so the summary the user reads is provably
// the set of changes applied.
type Change struct {
	Path string
	// Kind is "create", "update", or "unchanged".
	Kind string
	// Detail lines describe what specifically differs.
	Detail []string
}

// Plan reports what setup-trace would change, without touching anything.
func Plan(o SetupOptions) ([]Change, error) {
	runscPath := o.RunscPath
	if runscPath == "" {
		p, err := lookRunsc()
		if err != nil {
			return nil, err
		}
		runscPath = p
	}

	var changes []Change

	// 1. The pod-init trace config.
	want, err := json.MarshalIndent(podInitConfig{
		TraceSession: traceConfig{
			Name:   "Default",
			Points: o.Points(),
			Sinks: []traceSink{{
				Name:   "remote",
				Config: map[string]any{"endpoint": o.sinkPath()},
				// True, deliberately: a sandbox must not refuse to boot
				// because Faultbox happens not to be running. The honesty this
				// gives up is recovered by failing any test whose watch()
				// observed nothing. See RFC-056.
				IgnoreSetupError: true,
			}},
		},
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal trace config: %w", err)
	}
	want = append(want, '\n')

	got, readErr := os.ReadFile(o.traceConfig())
	switch {
	case readErr != nil:
		changes = append(changes, Change{
			Path: o.traceConfig(), Kind: "create",
			Detail: describeSession(o),
		})
	case string(got) != string(want):
		changes = append(changes, Change{
			Path: o.traceConfig(), Kind: "update",
			Detail: append([]string{"trace session replaced:"}, describeSession(o)...),
		})
	default:
		changes = append(changes, Change{Path: o.traceConfig(), Kind: "unchanged"})
	}

	// 2. The daemon.json runtime entry.
	daemon, err := readDaemonJSON(o.daemonJSON())
	if err != nil {
		return nil, err
	}
	runtimes, _ := daemon["runtimes"].(map[string]any)
	existing, present := runtimes[TraceRuntimeName]

	wantEntry := map[string]any{
		"path":        runscPath,
		"runtimeArgs": []any{"--pod-init-config=" + o.traceConfig()},
	}

	switch {
	case !present:
		changes = append(changes, Change{
			Path: o.daemonJSON(), Kind: "update",
			Detail: []string{
				fmt.Sprintf("+ runtimes.%q.path        = %q", TraceRuntimeName, runscPath),
				fmt.Sprintf("+ runtimes.%q.runtimeArgs = [\"--pod-init-config=%s\"]", TraceRuntimeName, o.traceConfig()),
				preservedNote(runtimes),
			},
		})
	case !sameEntry(existing, wantEntry):
		changes = append(changes, Change{
			Path: o.daemonJSON(), Kind: "update",
			Detail: []string{
				fmt.Sprintf("~ runtimes.%q replaced (it pointed somewhere else)", TraceRuntimeName),
				fmt.Sprintf("  path        = %q", runscPath),
				fmt.Sprintf("  runtimeArgs = [\"--pod-init-config=%s\"]", o.traceConfig()),
				preservedNote(runtimes),
			},
		})
	default:
		changes = append(changes, Change{Path: o.daemonJSON(), Kind: "unchanged"})
	}

	return changes, nil
}

// Apply performs the changes Plan describes and returns them.
//
// Returns the same slice Plan would, so the caller reports exactly what
// happened rather than what was intended.
func Apply(o SetupOptions) ([]Change, error) {
	changes, err := Plan(o)
	if err != nil {
		return nil, err
	}
	if Unchanged(changes) {
		return changes, nil
	}

	runscPath := o.RunscPath
	if runscPath == "" {
		p, err := lookRunsc()
		if err != nil {
			return nil, err
		}
		runscPath = p
	}

	// Trace config.
	body, err := json.MarshalIndent(podInitConfig{
		TraceSession: traceConfig{
			Name:   "Default",
			Points: o.Points(),
			Sinks: []traceSink{{
				Name:             "remote",
				Config:           map[string]any{"endpoint": o.sinkPath()},
				IgnoreSetupError: true,
			}},
		},
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal trace config: %w", err)
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(o.traceConfig()), 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(o.traceConfig()), err)
	}
	if err := os.WriteFile(o.traceConfig(), body, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w (needs root)", o.traceConfig(), err)
	}

	// daemon.json — read-modify-write, preserving every key we do not own.
	daemon, err := readDaemonJSON(o.daemonJSON())
	if err != nil {
		return nil, err
	}
	runtimes, _ := daemon["runtimes"].(map[string]any)
	if runtimes == nil {
		runtimes = map[string]any{}
	}
	runtimes[TraceRuntimeName] = map[string]any{
		"path":        runscPath,
		"runtimeArgs": []any{"--pod-init-config=" + o.traceConfig()},
	}
	daemon["runtimes"] = runtimes

	out, err := json.MarshalIndent(daemon, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("marshal daemon.json: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(o.daemonJSON(), out, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w (needs root)", o.daemonJSON(), err)
	}
	return changes, nil
}

// Unchanged reports whether every change is a no-op, which is what makes
// re-running setup-trace safe.
func Unchanged(changes []Change) bool {
	for _, c := range changes {
		if c.Kind != "unchanged" {
			return false
		}
	}
	return true
}

// InstalledPoints reports the trace points the host config currently requests,
// so a spec asking for an op the session cannot deliver fails at load rather
// than observing nothing. Returns nil when no config is installed.
func InstalledPoints(path string) ([]string, error) {
	if path == "" {
		path = TraceConfigPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg podInitConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	names := make([]string, 0, len(cfg.TraceSession.Points))
	for _, p := range cfg.TraceSession.Points {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names, nil
}

// lookRunsc resolves the runsc binary, with an error that names the fix
// rather than reporting a bare "not found".
func lookRunsc() (string, error) {
	p, err := exec.LookPath(RuntimeName)
	if err != nil {
		return "", fmt.Errorf(
			"runsc is not on PATH, so the trace runtime cannot be registered. "+
				"Install it from https://gvisor.dev/docs/user_guide/install/ and re-run: %w", err)
	}
	return p, nil
}

func readDaemonJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A host with no daemon.json is normal; Docker defaults apply.
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// Refuse rather than overwrite: this file may be hand-maintained or
		// generated, and clobbering it would be a far worse outcome than
		// declining to install.
		return nil, fmt.Errorf("%s is not valid JSON (%w); refusing to rewrite it", path, err)
	}
	return m, nil
}

func sameEntry(got any, want map[string]any) bool {
	g, ok := got.(map[string]any)
	if !ok {
		return false
	}
	if g["path"] != want["path"] {
		return false
	}
	ga, _ := g["runtimeArgs"].([]any)
	wa, _ := want["runtimeArgs"].([]any)
	if len(ga) != len(wa) {
		return false
	}
	for i := range ga {
		if fmt.Sprint(ga[i]) != fmt.Sprint(wa[i]) {
			return false
		}
	}
	return true
}

// preservedNote names the runtimes left untouched, because "what did this do
// to my Docker config" is the question the output exists to answer.
func preservedNote(runtimes map[string]any) string {
	var others []string
	for name := range runtimes {
		if name != TraceRuntimeName {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		return "  (no other runtimes were present)"
	}
	sort.Strings(others)
	return "  (left unchanged: " + strings.Join(others, ", ") + ")"
}

func describeSession(o SetupOptions) []string {
	pts := o.Points()
	names := make([]string, 0, len(pts))
	for _, p := range pts {
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(p.Name, "syscall/"), "/exit"))
	}
	return []string{
		fmt.Sprintf("  trace session \"Default\", %d points: %s", len(pts), strings.Join(names, ", ")),
		fmt.Sprintf("  sink: %s", o.sinkPath()),
		"  ignore_setup_error: true — a sandbox still boots when Faultbox is not running",
	}
}
