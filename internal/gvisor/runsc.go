// Package gvisor drives the stock runsc binary: availability checks and
// trace-session control for RFC-054's filesystem observation.
//
// Nothing here forks or patches gVisor. runsc is used exactly as shipped, and
// the Docker default runtime stays runc — a spec opts in per-run.
package gvisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RuntimeName is the Docker runtime a spec must register to use gVisor.
const RuntimeName = "runsc"

// dockerRuntimeRoot is where Docker keeps OCI runtime state. runsc needs it to
// find a sandbox by container ID.
const dockerRuntimeRoot = "/var/run/docker/runtime-runc/moby"

// Availability reports whether this host can run gVisor-backed observation.
type Availability struct {
	// BinaryPath is where runsc was found, empty if absent.
	BinaryPath string
	// Version is runsc's reported version.
	Version string
	// DockerRegistered reports whether the daemon knows the "runsc" runtime.
	DockerRegistered bool
	// Err explains the first thing that is missing, with the fix.
	Err error
}

// OK reports whether everything needed is present.
func (a Availability) OK() bool { return a.Err == nil }

// CheckAvailability probes for runsc and its Docker registration.
//
// Called at spec load rather than at service start (RFC-054 open question 5):
// "runsc is not installed" is far more actionable before anything has been
// launched than as a container that mysteriously fails to create.
func CheckAvailability(ctx context.Context) Availability {
	var a Availability

	path, err := exec.LookPath(RuntimeName)
	if err != nil {
		a.Err = fmt.Errorf("runsc is not on PATH, so filesystem observation is unavailable. " +
			"Install it from https://gvisor.dev/docs/user_guide/install/ and register it with " +
			"`sudo runsc install`, then restart Docker")
		return a
	}
	a.BinaryPath = path

	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		a.Err = fmt.Errorf("runsc at %s did not report a version (%w); the install may be broken", path, err)
		return a
	}
	a.Version = firstLine(string(out))

	registered, err := dockerHasRuntime(ctx)
	if err != nil {
		// Docker may simply not be reachable; that is a separate failure the
		// container layer reports better than this probe can.
		a.DockerRegistered = false
		return a
	}
	a.DockerRegistered = registered
	if !registered {
		a.Err = fmt.Errorf("runsc is installed at %s but Docker does not have a %q runtime registered. "+
			"Run `sudo runsc install` and restart the Docker daemon", path, RuntimeName)
	}
	return a
}

// dockerHasRuntime asks the daemon whether the runsc runtime is registered.
func dockerHasRuntime(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .Runtimes}}").Output()
	if err != nil {
		return false, err
	}
	var runtimes map[string]any
	if err := json.Unmarshal(out, &runtimes); err != nil {
		return false, err
	}
	_, ok := runtimes[RuntimeName]
	return ok, nil
}

// TraceSession is an active trace session on one sandbox.
type TraceSession struct {
	containerID string
	runscPath   string
	root        string
	configPath  string
}

// TracePoint is one point to enable.
type TracePoint struct {
	Name           string   `json:"name"`
	OptionalFields []string `json:"optional_fields,omitempty"`
	ContextFields  []string `json:"context_fields,omitempty"`
}

type traceSink struct {
	Name             string         `json:"name"`
	Config           map[string]any `json:"config"`
	IgnoreSetupError bool           `json:"ignore_setup_error"`
}

type traceConfig struct {
	// Name must be "Default". runsc rejects anything else with
	// "only a single \"Default\" session is supported" (M0.3 finding).
	Name   string       `json:"name"`
	Points []TracePoint `json:"points"`
	Sinks  []traceSink  `json:"sinks"`
}

// FileIOPoints returns the trace points needed for filesystem observation.
//
// fd_path is an *optional* field and must be requested explicitly or it
// arrives empty — which would leave every path-filtered watch matching nothing
// (M0.3 finding). cwd is requested for openat, whose pathname is frequently
// relative and needs assembling.
func FileIOPoints() []TracePoint {
	common := []string{"time", "thread_id", "container_id", "process_name"}
	return []TracePoint{
		{Name: "syscall/openat/exit", OptionalFields: []string{"fd_path"}, ContextFields: append([]string{"cwd"}, common...)},
		{Name: "syscall/write/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/pwrite64/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/writev/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/read/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/pread64/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/close/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
		{Name: "syscall/connect/exit", OptionalFields: []string{"fd_path"}, ContextFields: common},
	}
}

// StartTrace attaches a trace session to a running sandbox, streaming points
// to sinkPath.
//
// The sink must already be listening: with ignore_setup_error=false the Sentry
// refuses to proceed if it cannot connect, which is the behaviour we want —
// a silently absent sink would mean a watch() that observes nothing.
func StartTrace(ctx context.Context, containerID, sinkPath string, points []TracePoint) (*TraceSession, error) {
	runscPath, err := exec.LookPath(RuntimeName)
	if err != nil {
		return nil, fmt.Errorf("runsc not found: %w", err)
	}

	cfg := traceConfig{
		Name:   "Default",
		Points: points,
		Sinks: []traceSink{{
			Name:             "remote",
			Config:           map[string]any{"endpoint": sinkPath},
			IgnoreSetupError: false,
		}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal trace config: %w", err)
	}
	f, err := os.CreateTemp("", "faultbox-trace-*.json")
	if err != nil {
		return nil, fmt.Errorf("create trace config: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("write trace config: %w", err)
	}
	f.Close()

	s := &TraceSession{
		containerID: containerID,
		runscPath:   runscPath,
		root:        dockerRuntimeRoot,
		configPath:  f.Name(),
	}
	out, err := exec.CommandContext(ctx, runscPath,
		"--root", s.root, "trace", "create", "--config", s.configPath, containerID).CombinedOutput()
	if err != nil {
		os.Remove(s.configPath)
		return nil, fmt.Errorf("runsc trace create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return s, nil
}

// Stop deletes the trace session and removes the temporary config.
func (s *TraceSession) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	defer os.Remove(s.configPath)
	out, err := exec.CommandContext(ctx, s.runscPath,
		"--root", s.root, "trace", "delete", "--name", "Default", s.containerID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runsc trace delete: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
