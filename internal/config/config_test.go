package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTopology_NewFormat(t *testing.T) {
	yaml := `
version: "1"
services:
  db:
    binary: /usr/bin/db
    interfaces:
      main:
        protocol: tcp
        port: 5432
    healthcheck:
      test: tcp://localhost:5432
      timeout: 10s
  api:
    binary: /usr/bin/api
    interfaces:
      public:
        protocol: http
        port: 8080
    depends_on: [db]
    environment:
      DB_ADDR: "{{db.main.addr}}"
    healthcheck:
      test: http://localhost:8080/health
      timeout: 10s
`
	cfg := mustLoadTopologyString(t, yaml)

	// Check db service.
	db := cfg.Services["db"]
	if db.Binary != "/usr/bin/db" {
		t.Fatalf("db.Binary = %q, want /usr/bin/db", db.Binary)
	}
	if db.Interfaces["main"].Protocol != "tcp" {
		t.Fatalf("db interface protocol = %q, want tcp", db.Interfaces["main"].Protocol)
	}
	if db.Interfaces["main"].Port != 5432 {
		t.Fatalf("db interface port = %d, want 5432", db.Interfaces["main"].Port)
	}
	if db.Healthcheck == nil || db.Healthcheck.Test != "tcp://localhost:5432" {
		t.Fatalf("db healthcheck = %v, want tcp://localhost:5432", db.Healthcheck)
	}

	// Check api service.
	api := cfg.Services["api"]
	if len(api.DependsOn) != 1 || api.DependsOn[0] != "db" {
		t.Fatalf("api depends_on = %v, want [db]", api.DependsOn)
	}
	if api.Environment["DB_ADDR"] != "{{db.main.addr}}" {
		t.Fatalf("api env DB_ADDR = %q, want template", api.Environment["DB_ADDR"])
	}
}

func TestLoadTopology_LegacyFormat(t *testing.T) {
	yaml := `
version: "1"
services:
  db:
    binary: /usr/bin/db
    port: 5432
    env:
      PORT: "5432"
    ready: tcp://localhost:5432
`
	cfg := mustLoadTopologyString(t, yaml)
	db := cfg.Services["db"]

	// Legacy port should migrate to interfaces.default.
	if db.Interfaces["default"].Port != 5432 {
		t.Fatalf("migrated port = %d, want 5432", db.Interfaces["default"].Port)
	}
	if db.Interfaces["default"].Protocol != "tcp" {
		t.Fatalf("migrated protocol = %q, want tcp", db.Interfaces["default"].Protocol)
	}

	// Legacy env should migrate to environment.
	if db.Environment["PORT"] != "5432" {
		t.Fatalf("migrated env PORT = %q, want 5432", db.Environment["PORT"])
	}

	// Legacy ready should migrate to healthcheck.
	if db.Healthcheck == nil || db.Healthcheck.Test != "tcp://localhost:5432" {
		t.Fatalf("migrated healthcheck = %v", db.Healthcheck)
	}
}

func TestLoadTopology_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no services",
			yaml: `version: "1"
services: {}`,
			want: "no services defined",
		},
		{
			name: "missing binary",
			yaml: `version: "1"
services:
  db:
    interfaces:
      main: { protocol: tcp, port: 5432 }`,
			want: "binary is required",
		},
		{
			name: "missing interface port",
			yaml: `version: "1"
services:
  db:
    binary: /usr/bin/db
    interfaces:
      main: { protocol: tcp }`,
			want: "port is required",
		},
		{
			name: "missing interface protocol",
			yaml: `version: "1"
services:
  db:
    binary: /usr/bin/db
    interfaces:
      main: { port: 5432 }`,
			want: "protocol is required",
		},
		{
			name: "bad depends_on",
			yaml: `version: "1"
services:
  api:
    binary: /usr/bin/api
    interfaces:
      main: { protocol: http, port: 8080 }
    depends_on: [nonexistent]`,
			want: "depends_on \"nonexistent\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadTopologyString(tt.yaml)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q should contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadSpec_WithSteps(t *testing.T) {
	yaml := `
version: "1"
system: faultbox.yaml
traces:
  test-trace:
    description: "test"
    faults:
      db:
        - "write=delay:500ms:100%"
        - syscall: connect
          action: deny
          errno: ECONNREFUSED
          probability: "100%"
    steps:
      - api.post:
          path: /data/key1
          body: "value1"
          expect:
            status: 200
      - db.main.send:
          data: "PING"
          expect:
            equals: "PONG"
      - sleep: 1s
    assert:
      - exit_code: { service: api, equals: 0 }
    timeout: "15s"
`
	cfg := mustLoadSpecString(t, yaml)
	trace := cfg.Traces["test-trace"]

	// Check mixed fault formats.
	faults := trace.Faults["db"]
	if len(faults) != 2 {
		t.Fatalf("fault count = %d, want 2", len(faults))
	}
	// String form.
	if faults[0].Raw != "write=delay:500ms:100%" {
		t.Fatalf("fault[0].Raw = %q", faults[0].Raw)
	}
	// Object form.
	if faults[1].Syscall != "connect" || faults[1].Action != "deny" {
		t.Fatalf("fault[1] = %+v", faults[1])
	}

	// Check ToRuleStrings.
	rules := faults.ToRuleStrings()
	if rules[0] != "write=delay:500ms:100%" {
		t.Fatalf("rule[0] = %q", rules[0])
	}
	if !strings.Contains(rules[1], "connect=ECONNREFUSED:100%") {
		t.Fatalf("rule[1] = %q", rules[1])
	}

	// Check steps.
	if len(trace.Steps) != 3 {
		t.Fatalf("step count = %d, want 3", len(trace.Steps))
	}
	if trace.Steps[0].Action != "api.post" {
		t.Fatalf("step[0].Action = %q, want api.post", trace.Steps[0].Action)
	}
	if trace.Steps[1].Action != "db.main.send" {
		t.Fatalf("step[1].Action = %q, want db.main.send", trace.Steps[1].Action)
	}
	if trace.Steps[2].Sleep == nil {
		t.Fatal("step[2].Sleep is nil, want 1s")
	}
}

func TestResolveServiceAddr(t *testing.T) {
	cfg := &TopologyConfig{
		Services: map[string]ServiceConfig{
			"db": {
				Interfaces: map[string]InterfaceConfig{
					"main": {Protocol: "tcp", Port: 5432},
				},
			},
			"api": {
				Interfaces: map[string]InterfaceConfig{
					"public":   {Protocol: "http", Port: 8080},
					"internal": {Protocol: "grpc", Port: 9090},
				},
			},
		},
	}

	// Single interface — no name needed.
	addr, err := ResolveServiceAddr(cfg, "db", "")
	if err != nil {
		t.Fatalf("resolve db: %v", err)
	}
	if addr != "localhost:5432" {
		t.Fatalf("db addr = %q, want localhost:5432", addr)
	}

	// Multiple interfaces — name required.
	_, err = ResolveServiceAddr(cfg, "api", "")
	if err == nil {
		t.Fatal("expected error for ambiguous interface")
	}

	// Explicit interface.
	addr, err = ResolveServiceAddr(cfg, "api", "public")
	if err != nil {
		t.Fatalf("resolve api.public: %v", err)
	}
	if addr != "localhost:8080" {
		t.Fatalf("api.public addr = %q, want localhost:8080", addr)
	}
}

func TestResolveEnv(t *testing.T) {
	cfg := &TopologyConfig{
		Services: map[string]ServiceConfig{
			"db": {
				Interfaces: map[string]InterfaceConfig{
					"main": {Protocol: "tcp", Port: 5432},
				},
				Environment: map[string]string{"PORT": "5432"},
			},
			"api": {
				Interfaces: map[string]InterfaceConfig{
					"public": {Protocol: "http", Port: 8080},
				},
				Environment: map[string]string{
					"PORT":    "8080",
					"DB_ADDR": "{{db.main.addr}}",
				},
			},
		},
	}

	env, err := ResolveEnv(cfg, "api")
	if err != nil {
		t.Fatalf("resolve env: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		envMap[parts[0]] = parts[1]
	}

	// Check template resolution.
	if envMap["DB_ADDR"] != "localhost:5432" {
		t.Fatalf("DB_ADDR = %q, want localhost:5432", envMap["DB_ADDR"])
	}

	// Check auto-injected vars.
	if envMap["FAULTBOX_DB_MAIN_ADDR"] != "localhost:5432" {
		t.Fatalf("FAULTBOX_DB_MAIN_ADDR = %q", envMap["FAULTBOX_DB_MAIN_ADDR"])
	}
	if envMap["FAULTBOX_API_PUBLIC_PORT"] != "8080" {
		t.Fatalf("FAULTBOX_API_PUBLIC_PORT = %q", envMap["FAULTBOX_API_PUBLIC_PORT"])
	}
}

func TestDependencyOrder(t *testing.T) {
	services := map[string]ServiceConfig{
		"api":   {DependsOn: []string{"db", "cache"}},
		"db":    {},
		"cache": {DependsOn: []string{"db"}},
	}
	order, err := DependencyOrder(services)
	if err != nil {
		t.Fatalf("dependency order: %v", err)
	}

	indexOf := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}

	if indexOf("db") >= indexOf("cache") {
		t.Fatalf("db should come before cache: %v", order)
	}
	if indexOf("cache") >= indexOf("api") {
		t.Fatalf("cache should come before api: %v", order)
	}
}

func TestDependencyOrder_Circular(t *testing.T) {
	services := map[string]ServiceConfig{
		"a": {DependsOn: []string{"b"}},
		"b": {DependsOn: []string{"a"}},
	}
	_, err := DependencyOrder(services)
	if err == nil {
		t.Fatal("expected circular dependency error")
	}
}

// --- helpers ---

func mustLoadTopologyString(t *testing.T, content string) *TopologyConfig {
	t.Helper()
	cfg, err := loadTopologyString(content)
	if err != nil {
		t.Fatalf("load topology: %v", err)
	}
	return cfg
}

func loadTopologyString(content string) (*TopologyConfig, error) {
	dir, err := os.MkdirTemp("", "faultbox-test-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "faultbox.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return nil, err
	}
	return LoadTopology(path)
}

func mustLoadSpecString(t *testing.T, content string) *SpecConfig {
	t.Helper()
	dir, err := os.MkdirTemp("", "faultbox-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	cfg, err := LoadSpec(path)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return cfg
}
