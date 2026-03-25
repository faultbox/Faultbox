package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/faultbox/Faultbox/internal/config"
	"github.com/faultbox/Faultbox/internal/logging"
)

// SimulationResult captures the outcome of running all traces.
type SimulationResult struct {
	SimulationID string        `json:"simulation_id"`
	DurationMs   int64         `json:"duration_ms"`
	Traces       []TraceResult `json:"traces"`
	Pass         int           `json:"pass"`
	Fail         int           `json:"fail"`
}

// TraceResult captures the outcome of a single trace execution.
type TraceResult struct {
	Name       string                   `json:"name"`
	Result     string                   `json:"result"` // "pass" or "fail"
	Reason     string                   `json:"reason,omitempty"`
	DurationMs int64                    `json:"duration_ms"`
	Services   map[string]ServiceResult `json:"services"`
	Steps      []StepResult             `json:"steps,omitempty"`
}

// ServiceResult captures per-service outcome in a trace.
type ServiceResult struct {
	ExitCode      int `json:"exit_code"`
	FaultsApplied int `json:"faults_applied"`
}

// RunSimulation executes all traces from a spec against a topology.
// Services are restarted between traces for clean state.
func (e *Engine) RunSimulation(ctx context.Context, topo *config.TopologyConfig, spec *config.SpecConfig) (*SimulationResult, error) {
	simStart := time.Now()

	simID, err := generateSimID()
	if err != nil {
		return nil, err
	}

	log := logging.WithComponent(e.log, "simulation")
	log.Info("starting simulation",
		slog.String("simulation_id", simID),
		slog.Int("services", len(topo.Services)),
		slog.Int("traces", len(spec.Traces)),
	)

	// Resolve trace execution order (map iteration is random, use sorted keys).
	traceNames := sortedKeys(spec.Traces)

	result := &SimulationResult{
		SimulationID: simID,
		Traces:       make([]TraceResult, 0, len(traceNames)),
	}

	for _, traceName := range traceNames {
		traceCfg := spec.Traces[traceName]

		// Wait for ports to be freed before starting services.
		waitPortsFree(topo, 10*time.Second)

		log.Info("executing trace",
			slog.String("trace", traceName),
			slog.String("description", traceCfg.Description),
		)

		tr, err := e.runTrace(ctx, topo, traceName, &traceCfg, log)
		if err != nil {
			return nil, fmt.Errorf("trace %q: %w", traceName, err)
		}

		result.Traces = append(result.Traces, *tr)
		if tr.Result == "pass" {
			result.Pass++
		} else {
			result.Fail++
		}

		log.Info("trace completed",
			slog.String("trace", traceName),
			slog.String("result", tr.Result),
			slog.Int64("duration_ms", tr.DurationMs),
		)
	}

	result.DurationMs = time.Since(simStart).Milliseconds()

	log.Info("simulation completed",
		slog.String("simulation_id", simID),
		slog.Int("pass", result.Pass),
		slog.Int("fail", result.Fail),
		slog.Int64("duration_ms", result.DurationMs),
	)

	return result, nil
}

// runTrace starts all services, runs steps, checks assertions.
func (e *Engine) runTrace(ctx context.Context, topo *config.TopologyConfig, name string, trace *config.TraceConfig, log *slog.Logger) (*TraceResult, error) {
	traceStart := time.Now()

	// Resolve dependency order.
	order, err := config.DependencyOrder(topo.Services)
	if err != nil {
		return nil, err
	}

	// Determine trace timeout.
	timeout := 30 * time.Second
	if trace.Timeout.Duration > 0 {
		timeout = trace.Timeout.Duration
	}

	traceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Start services in dependency order.
	type runningService struct {
		name   string
		session *Session
		cancel context.CancelFunc
		done   chan *Result
	}
	var running []runningService

	// Cleanup: stop all services on return (if not already stopped).
	defer func() {
		for _, rs := range running {
			rs.cancel()
			select {
			case <-rs.done:
			case <-time.After(5 * time.Second):
			}
		}
	}()

	for _, svcName := range order {
		svcCfg := topo.Services[svcName]

		// Build fault rules for this service in this trace.
		var faultRules []FaultRule
		if faultSet, ok := trace.Faults[svcName]; ok {
			faultRules, err = ParseFaultRules(faultSet.ToRuleStrings())
			if err != nil {
				return nil, fmt.Errorf("service %q faults: %w", svcName, err)
			}
		}

		// Resolve env vars with auto-injection and templates.
		envVars, err := config.ResolveEnv(topo, svcName)
		if err != nil {
			return nil, fmt.Errorf("service %q env: %w", svcName, err)
		}

		// Multi-service: skip NET namespace (shared host network).
		ns := NamespaceConfig{
			PID:   true,
			Mount: true,
			User:  true,
		}

		sessCfg := SessionConfig{
			Binary:     svcCfg.Binary,
			Args:       svcCfg.Args,
			Env:        envVars,
			Stdout:     os.Stdout,
			Stderr:     os.Stderr,
			Namespaces: ns,
			FaultRules: faultRules,
		}

		svcLog := log.With(slog.String("service", svcName))
		session, err := NewSession(sessCfg, svcLog)
		if err != nil {
			return nil, fmt.Errorf("create session for %q: %w", svcName, err)
		}

		svcCtx, svcCancel := context.WithCancel(traceCtx)
		done := make(chan *Result, 1)
		go func() {
			r, _ := session.Run(svcCtx)
			done <- r
		}()

		running = append(running, runningService{
			name:    svcName,
			session: session,
			cancel:  svcCancel,
			done:    done,
		})

		// Wait for readiness.
		if svcCfg.Healthcheck != nil {
			hcTimeout := svcCfg.Healthcheck.Timeout.Duration
			if hcTimeout <= 0 {
				hcTimeout = 10 * time.Second
			}
			if err := waitReady(traceCtx, svcCfg.Healthcheck.Test, hcTimeout, svcLog); err != nil {
				return &TraceResult{
					Name:       name,
					Result:     "fail",
					Reason:     fmt.Sprintf("service %q not ready: %v", svcName, err),
					DurationMs: time.Since(traceStart).Milliseconds(),
				}, nil
			}
			svcLog.Info("service ready", slog.String("check", svcCfg.Healthcheck.Test))
		}
	}

	// All services are running.
	trResult := &TraceResult{
		Name:     name,
		Result:   "pass",
		Services: make(map[string]ServiceResult),
	}

	// Run steps (active scenario driver).
	if len(trace.Steps) > 0 {
		stepLog := log.With(slog.String("phase", "steps"))
		stepResults, err := RunSteps(traceCtx, topo, trace.Steps, stepLog)
		trResult.Steps = stepResults
		if err != nil {
			trResult.Result = "fail"
			trResult.Reason = fmt.Sprintf("step execution error: %v", err)
		} else {
			// Check if any step failed.
			for _, sr := range stepResults {
				if !sr.Success {
					trResult.Result = "fail"
					trResult.Reason = fmt.Sprintf("step %q failed: %s", sr.Action, sr.Error)
					break
				}
			}
		}
	}

	// Run eventually assertions (while services are still alive).
	if trResult.Result == "pass" {
		for _, a := range trace.Assert {
			if a.Eventually != nil && a.Eventually.HTTP != nil {
				check := a.Eventually.HTTP
				evTimeout := 5 * time.Second
				if a.Eventually.Timeout.Duration > 0 {
					evTimeout = a.Eventually.Timeout.Duration
				}
				if err := waitHTTP(traceCtx, check.URL, check.Status, evTimeout); err != nil {
					trResult.Result = "fail"
					trResult.Reason = fmt.Sprintf("assertion: eventually http %s status=%d: %v",
						check.URL, check.Status, err)
					break
				}
			}
		}
	}

	// Stop all services and collect exit codes.
	// First, check which services already exited on their own (before we kill them).
	alreadyExited := make(map[string]*Result)
	for _, rs := range running {
		select {
		case r := <-rs.done:
			alreadyExited[rs.name] = r
		default:
			// Still running — will be killed below.
		}
	}
	for _, rs := range running {
		rs.cancel()
	}
	for _, rs := range running {
		exitCode := 0
		if r, exited := alreadyExited[rs.name]; exited {
			// Service exited on its own before we cancelled — use real exit code.
			if r != nil {
				exitCode = r.ExitCode
			}
		} else {
			// Service was still running — we killed it. Wait for it to finish.
			select {
			case <-rs.done:
			case <-time.After(5 * time.Second):
			}
			// Killed by us = orderly shutdown = exit code 0.
			exitCode = 0
		}
		faultCount := 0
		if faults, ok := trace.Faults[rs.name]; ok {
			faultCount = len(faults)
		}
		trResult.Services[rs.name] = ServiceResult{
			ExitCode:      exitCode,
			FaultsApplied: faultCount,
		}
	}
	running = nil

	// Check exit code assertions (after services stopped).
	if trResult.Result == "pass" {
		for _, a := range trace.Assert {
			if a.ExitCode != nil {
				svcResult, ok := trResult.Services[a.ExitCode.Service]
				if !ok {
					trResult.Result = "fail"
					trResult.Reason = fmt.Sprintf("assertion: service %q not found", a.ExitCode.Service)
					break
				}
				if svcResult.ExitCode != a.ExitCode.Equals {
					trResult.Result = "fail"
					trResult.Reason = fmt.Sprintf("assertion: %s.exit_code == %d, got %d",
						a.ExitCode.Service, a.ExitCode.Equals, svcResult.ExitCode)
					break
				}
			}
		}
	}

	trResult.DurationMs = time.Since(traceStart).Milliseconds()
	return trResult, nil
}

// waitReady polls a readiness check until it succeeds or times out.
func waitReady(ctx context.Context, check string, timeout time.Duration, log *slog.Logger) error {
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

// waitHTTP polls an HTTP endpoint until the expected status is returned.
func waitHTTP(ctx context.Context, url string, expectedStatus int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == expectedStatus {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// WriteResults writes simulation results to a JSON file.
func WriteResults(path string, result *SimulationResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write results to %s: %w", path, err)
	}
	return nil
}

func generateSimID() (string, error) {
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("generate simulation ID: %w", err)
	}
	return id, nil
}

// waitPortsFree waits until all service ports are available.
func waitPortsFree(topo *config.TopologyConfig, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allFree := true
		for _, svc := range topo.Services {
			for _, iface := range svc.Interfaces {
				if iface.Port > 0 {
					conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", iface.Port), 100*time.Millisecond)
					if err == nil {
						conn.Close()
						allFree = false
						break
					}
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
