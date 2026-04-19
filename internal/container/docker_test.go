package container

import (
	"encoding/json"
	"testing"
)

func TestShimConfigJSON(t *testing.T) {
	cfg := ShimConfig{
		SyscallNrs: []uint32{257, 1, 42},
		Entrypoint: []string{"/usr/bin/postgres"},
		Cmd:        []string{"-c", "listen_addresses=*"},
		ReportPath: "/var/run/faultbox/listener-fd",
	}

	raw := ShimConfigJSON(cfg)

	// Verify it's valid JSON.
	var decoded ShimConfig
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("ShimConfigJSON produced invalid JSON: %v", err)
	}

	if len(decoded.SyscallNrs) != 3 || decoded.SyscallNrs[0] != 257 {
		t.Fatalf("SyscallNrs = %v, want [257 1 42]", decoded.SyscallNrs)
	}
	if len(decoded.Entrypoint) != 1 || decoded.Entrypoint[0] != "/usr/bin/postgres" {
		t.Fatalf("Entrypoint = %v", decoded.Entrypoint)
	}
	if len(decoded.Cmd) != 2 || decoded.Cmd[1] != "listen_addresses=*" {
		t.Fatalf("Cmd = %v", decoded.Cmd)
	}
	if decoded.ReportPath != "/var/run/faultbox/listener-fd" {
		t.Fatalf("ReportPath = %q", decoded.ReportPath)
	}
}

func TestShimConfigJSONEmpty(t *testing.T) {
	cfg := ShimConfig{}
	raw := ShimConfigJSON(cfg)

	var decoded ShimConfig
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("empty ShimConfig JSON: %v", err)
	}
	if decoded.SyscallNrs != nil {
		t.Fatalf("expected nil SyscallNrs, got %v", decoded.SyscallNrs)
	}
}

func TestCreateOptsPortMapping(t *testing.T) {
	opts := CreateOpts{
		Name:  "test-svc",
		Image: "postgres:16",
		Ports: map[int]int{5432: 0, 8080: 9090},
	}

	// Verify port map structure.
	if len(opts.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(opts.Ports))
	}
	if opts.Ports[5432] != 0 {
		t.Fatalf("port 5432 should map to 0 (auto), got %d", opts.Ports[5432])
	}
	if opts.Ports[8080] != 9090 {
		t.Fatalf("port 8080 should map to 9090, got %d", opts.Ports[8080])
	}
}

func TestLaunchConfigValidation(t *testing.T) {
	cfg := LaunchConfig{
		Name:  "test",
		Image: "postgres:16",
		Ports: map[int]int{5432: 0},
	}

	if cfg.Name != "test" || cfg.Image != "postgres:16" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(cfg.Ports))
	}
}

// TestLaunchConfigNoSeccompField is a compile-time guarantee that the
// NoSeccomp field exists on LaunchConfig. v0.8.5 added it as a
// workaround for multi-process container entrypoints where the
// seccomp shim handoff hangs out the 3-minute test deadline; the
// runtime passes through `svc.NoSeccomp` sourced from `seccomp=False`
// in the Starlark spec.
func TestLaunchConfigNoSeccompField(t *testing.T) {
	cfg := LaunchConfig{
		Name:      "mysql",
		Image:     "mysql:8",
		NoSeccomp: true,
	}
	if !cfg.NoSeccomp {
		t.Fatal("NoSeccomp should round-trip through struct literal")
	}
}
