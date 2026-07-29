package gvisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOpts(t *testing.T, extra ...PointSet) SetupOptions {
	t.Helper()
	dir := t.TempDir()
	return SetupOptions{
		Extra:       extra,
		RunscPath:   "/usr/local/bin/runsc", // avoid depending on the host
		DaemonJSON:  filepath.Join(dir, "daemon.json"),
		TraceConfig: filepath.Join(dir, "trace.json"),
		SinkPath:    "/run/faultbox/seccheck.sock",
	}
}

// Re-running must be a no-op. A setup command that appends a duplicate entry
// every time, or reports work it did not do, stops being safe to put in a
// provisioning script.
func TestApplyIsIdempotent(t *testing.T) {
	o := testOpts(t)

	first, err := Apply(o)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if Unchanged(first) {
		t.Fatal("first Apply reported no changes on an empty host")
	}

	second, err := Apply(o)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if !Unchanged(second) {
		t.Errorf("second Apply reported changes; setup-trace is not idempotent: %+v", second)
	}
}

// The single most damaging thing this command could do is lose part of
// someone's Docker configuration.
func TestApplyPreservesEverythingElse(t *testing.T) {
	o := testOpts(t)
	pre := map[string]any{
		"runtimes": map[string]any{
			"runsc":  map[string]any{"path": "/usr/local/bin/runsc"},
			"kata":   map[string]any{"path": "/usr/bin/kata-runtime"},
			"nvidia": map[string]any{"path": "/usr/bin/nvidia-container-runtime"},
		},
		"log-driver":          "json-file",
		"storage-driver":      "overlay2",
		"default-runtime":     "runc",
		"insecure-registries": []any{"registry.internal:5000"},
	}
	raw, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(o.DaemonJSON, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(o); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := map[string]any{}
	body, _ := os.ReadFile(o.DaemonJSON)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	for _, key := range []string{"log-driver", "storage-driver", "default-runtime", "insecure-registries"} {
		if _, ok := got[key]; !ok {
			t.Errorf("top-level key %q was dropped", key)
		}
	}
	rts, _ := got["runtimes"].(map[string]any)
	for _, name := range []string{"runsc", "kata", "nvidia"} {
		if _, ok := rts[name]; !ok {
			t.Errorf("pre-existing runtime %q was dropped", name)
		}
	}
	if _, ok := rts[TraceRuntimeName]; !ok {
		t.Errorf("%q was not added", TraceRuntimeName)
	}
	// The user's own runsc entry must be untouched — they may run gVisor for
	// reasons unrelated to Faultbox.
	if rsc, _ := rts["runsc"].(map[string]any); rsc["runtimeArgs"] != nil {
		t.Errorf("the pre-existing runsc runtime was modified: %+v", rsc)
	}
}

// A daemon.json that does not parse must stop the command, not be replaced.
// It may be hand-maintained or generated, and clobbering it is far worse than
// declining to install.
func TestApplyRefusesToClobberUnparseableConfig(t *testing.T) {
	o := testOpts(t)
	original := "{ this is not json"
	if err := os.WriteFile(o.DaemonJSON, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Apply(o)
	if err == nil {
		t.Fatal("Apply must refuse a daemon.json it cannot parse")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should say it is refusing rather than failing obscurely: %v", err)
	}
	body, _ := os.ReadFile(o.DaemonJSON)
	if string(body) != original {
		t.Errorf("the unparseable file was modified; it must be left exactly as found:\n%s", body)
	}
}

func TestPlanWritesNothing(t *testing.T) {
	o := testOpts(t)
	changes, err := Plan(o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if Unchanged(changes) {
		t.Error("Plan should report pending changes on an empty host")
	}
	for _, p := range []string{o.DaemonJSON, o.TraceConfig} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Plan created %s; it must only report", p)
		}
	}
}

// The default set must NOT include read: measured at ~2x traffic and 1,488
// dropped points on a read-heavy workload, and drops fail tests.
func TestDefaultPointSetExcludesRead(t *testing.T) {
	pts := SetupOptions{}.Points()
	names := map[string]bool{}
	for _, p := range pts {
		names[p.Name] = true
	}
	for _, want := range []string{
		"syscall/openat/exit", "syscall/write/exit",
		"syscall/pwrite64/exit", "syscall/writev/exit",
	} {
		if !names[want] {
			t.Errorf("default set is missing %s", want)
		}
	}
	for _, unwanted := range []string{
		"syscall/read/exit", "syscall/pread64/exit",
		"syscall/close/exit", "syscall/connect/exit",
	} {
		if names[unwanted] {
			t.Errorf("default set includes %s; it must be opt-in (drop risk)", unwanted)
		}
	}
}

func TestExtraPointSetsAreAdditive(t *testing.T) {
	base := len(SetupOptions{}.Points())
	withRead := SetupOptions{Extra: []PointSet{PointSetRead}}.Points()
	if len(withRead) != base+2 {
		t.Errorf("--with-read should add read and pread64: %d -> %d", base, len(withRead))
	}
	// Asking twice must not duplicate: a duplicated point is a duplicated
	// event stream and double the drop pressure.
	twice := SetupOptions{Extra: []PointSet{PointSetRead, PointSetRead}}.Points()
	if len(twice) != len(withRead) {
		t.Errorf("repeating a point set duplicated points: %d vs %d", len(twice), len(withRead))
	}
}

// A stale registration — one pointing at a different config path — must be
// detected and replaced, not silently accepted.
func TestStaleRegistrationIsDetected(t *testing.T) {
	o := testOpts(t)
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}

	// Simulate an entry left by an older install pointing elsewhere.
	body, _ := os.ReadFile(o.DaemonJSON)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	rts := m["runtimes"].(map[string]any)
	rts[TraceRuntimeName] = map[string]any{
		"path":        "/usr/local/bin/runsc",
		"runtimeArgs": []any{"--pod-init-config=/old/path.json"},
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(o.DaemonJSON, out, 0o644)

	changes, err := Plan(o)
	if err != nil {
		t.Fatal(err)
	}
	if Unchanged(changes) {
		t.Fatal("a registration pointing at the wrong config was reported as up to date")
	}
	var found bool
	for _, c := range changes {
		if c.Path == o.DaemonJSON && c.Kind == "update" {
			found = true
			if !strings.Contains(strings.Join(c.Detail, " "), "pointed somewhere else") {
				t.Errorf("the report should say why it is replacing the entry: %v", c.Detail)
			}
		}
	}
	if !found {
		t.Error("no update planned for the stale daemon.json entry")
	}
}

// Changing the requested points must be noticed — otherwise `--with-read`
// would appear to work while the installed session still lacked read.
func TestChangingPointsIsDetected(t *testing.T) {
	o := testOpts(t)
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}
	withRead := o
	withRead.Extra = []PointSet{PointSetRead}

	changes, err := Plan(withRead)
	if err != nil {
		t.Fatal(err)
	}
	if Unchanged(changes) {
		t.Fatal("adding --with-read to an existing install reported no change")
	}
}

func TestInstalledPointsReportsWhatIsOnDisk(t *testing.T) {
	o := testOpts(t, PointSetClose)
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledPoints(o.TraceConfig)
	if err != nil {
		t.Fatalf("InstalledPoints: %v", err)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "syscall/close/exit") {
		t.Errorf("close was installed but not reported: %v", got)
	}
	if strings.Contains(joined, "syscall/read/exit") {
		t.Errorf("read was not installed but is reported: %v", got)
	}
}

// The sink must tolerate being absent, or a stale registration would stop
// every gVisor container on the host whenever Faultbox is not running.
func TestTraceConfigToleratesMissingSink(t *testing.T) {
	o := testOpts(t)
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(o.TraceConfig)
	if err != nil {
		t.Fatal(err)
	}
	var cfg podInitConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.TraceSession.Sinks) != 1 {
		t.Fatalf("expected one sink, got %d", len(cfg.TraceSession.Sinks))
	}
	if !cfg.TraceSession.Sinks[0].IgnoreSetupError {
		t.Error("ignore_setup_error must be true; false makes an idle registration " +
			"break every gVisor container on the host")
	}
	if cfg.TraceSession.Name != "Default" {
		t.Errorf("runsc only accepts a session named \"Default\", got %q", cfg.TraceSession.Name)
	}
}
