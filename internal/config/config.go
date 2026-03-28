// Package config provides YAML parsing for Faultbox topology and spec files.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Topology (faultbox.yaml)
// ---------------------------------------------------------------------------

// TopologyConfig is parsed from faultbox.yaml — describes the system topology.
type TopologyConfig struct {
	Version  string                   `yaml:"version"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// ServiceConfig describes one service in the topology.
type ServiceConfig struct {
	Binary    string                     `yaml:"binary"`
	Args      []string                   `yaml:"args"`
	DependsOn []string                   `yaml:"depends_on"`

	// New: named interfaces (protocol + port).
	Interfaces map[string]InterfaceConfig `yaml:"interfaces"`

	// Healthcheck (Compose-compatible naming).
	Healthcheck *HealthcheckConfig `yaml:"healthcheck"`

	// Environment variables. Supports {{service.interface.addr}} templates.
	Environment map[string]string `yaml:"environment"`

	// --- Legacy fields (backward compat, migrated on load) ---
	Port  int               `yaml:"port"`
	Env   map[string]string `yaml:"env"`
	Ready string            `yaml:"ready"`
}

// InterfaceConfig describes one communication interface of a service.
type InterfaceConfig struct {
	Protocol string   `yaml:"protocol"` // "http", "tcp", "grpc", etc.
	Port     int      `yaml:"port"`
	Topics   []string `yaml:"topics,omitempty"` // for async protocols (kafka, etc.)
}

// HealthcheckConfig describes a service readiness check.
type HealthcheckConfig struct {
	Test     string   `yaml:"test"`     // "tcp://host:port" or "http://host/path"
	Interval Duration `yaml:"interval"` // poll interval (default 1s)
	Timeout  Duration `yaml:"timeout"`  // overall timeout (default 10s)
}

// ---------------------------------------------------------------------------
// Spec (spec.yaml)
// ---------------------------------------------------------------------------

// SpecConfig is parsed from spec.yaml — describes test traces.
type SpecConfig struct {
	Version string                 `yaml:"version"`
	System  string                 `yaml:"system"`
	Traces  map[string]TraceConfig `yaml:"traces"`
}

// TraceConfig describes one test trace.
type TraceConfig struct {
	Description string              `yaml:"description"`
	Faults      map[string]FaultSet `yaml:"faults"`
	Steps       []StepConfig        `yaml:"steps"`
	Assert      []AssertConfig      `yaml:"assert"`
	Timeout     Duration            `yaml:"timeout"`
}

// FaultSet holds fault rules for one service. Supports both string and object forms.
// YAML can be a list of strings, a list of maps, or a mix.
type FaultSet []FaultSpec

// FaultSpec holds one fault rule in either string or object form.
type FaultSpec struct {
	// Raw is the string form: "write=delay:500ms:100%"
	Raw string
	// Object form fields:
	Syscall     string  `yaml:"syscall"`
	Action      string  `yaml:"action"`      // "delay" or "deny"
	Errno       string  `yaml:"errno"`       // for deny
	Delay       string  `yaml:"delay"`       // for delay (duration string)
	Probability string  `yaml:"probability"` // "100%" or "0.5"
	PathGlob    string  `yaml:"path"`
	Trigger     *TriggerSpec `yaml:"trigger"`
}

// TriggerSpec is the object form of a stateful trigger.
type TriggerSpec struct {
	Type string `yaml:"type"` // "nth" or "after"
	N    int    `yaml:"n"`
}

// UnmarshalYAML handles both string and object forms for FaultSpec.
func (f *FaultSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		f.Raw = value.Value
		return nil
	}
	// Object form — decode into the struct fields.
	type plain FaultSpec
	return value.Decode((*plain)(f))
}

// ToRuleString converts a FaultSpec to the string form used by ParseFaultRule.
func (f *FaultSpec) ToRuleString() string {
	if f.Raw != "" {
		return f.Raw
	}
	// Build from object fields.
	var b strings.Builder
	b.WriteString(f.Syscall)
	b.WriteByte('=')

	if f.Action == "delay" {
		b.WriteString("delay:")
		b.WriteString(f.Delay)
		b.WriteByte(':')
		b.WriteString(f.Probability)
	} else {
		// deny
		errno := f.Errno
		if errno == "" {
			errno = "EIO"
		}
		b.WriteString(errno)
		b.WriteByte(':')
		b.WriteString(f.Probability)
		if f.PathGlob != "" {
			b.WriteByte(':')
			b.WriteString(f.PathGlob)
		}
	}

	if f.Trigger != nil {
		b.WriteByte(':')
		b.WriteString(f.Trigger.Type)
		b.WriteByte('=')
		b.WriteString(fmt.Sprintf("%d", f.Trigger.N))
	}
	return b.String()
}

// UnmarshalYAML for FaultSet handles a YAML sequence of mixed string/object items.
func (fs *FaultSet) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("faults must be a list, got %v", value.Kind)
	}
	specs := make([]FaultSpec, len(value.Content))
	for i, item := range value.Content {
		if err := item.Decode(&specs[i]); err != nil {
			return fmt.Errorf("fault[%d]: %w", i, err)
		}
	}
	*fs = specs
	return nil
}

// ToRuleStrings converts all FaultSpecs to string form.
func (fs FaultSet) ToRuleStrings() []string {
	result := make([]string, len(fs))
	for i := range fs {
		result[i] = fs[i].ToRuleString()
	}
	return result
}

// StepConfig represents one step in a trace scenario.
// Exactly one field should be set. The key format is "service[.interface].operation".
type StepConfig struct {
	// Sleep pauses execution for a duration.
	Sleep *Duration `yaml:"sleep,omitempty"`
	// Action is a service.operation or service.interface.operation step.
	// Populated during parsing from the dynamic YAML key.
	Action   string            `yaml:"-"`
	Args     map[string]any    `yaml:"-"`
}

// UnmarshalYAML handles the dynamic step format:
//   - sleep: 1s
//   - api.post: { path: /health, expect: { status: 200 } }
func (s *StepConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode || len(value.Content) < 2 {
		return fmt.Errorf("step must be a mapping with one key")
	}
	key := value.Content[0].Value
	val := value.Content[1]

	// Handle sleep specially.
	if key == "sleep" {
		var d Duration
		if err := val.Decode(&d); err != nil {
			return fmt.Errorf("step sleep: %w", err)
		}
		s.Sleep = &d
		return nil
	}

	// Dynamic step: key is "service.operation" or "service.interface.operation".
	s.Action = key
	args := make(map[string]any)
	if err := val.Decode(&args); err != nil {
		return fmt.Errorf("step %s args: %w", key, err)
	}
	s.Args = args
	return nil
}

// AssertConfig describes one assertion in a trace.
type AssertConfig struct {
	ExitCode   *ExitCodeAssert   `yaml:"exit_code"`
	Eventually *EventuallyAssert `yaml:"eventually"`
}

// ExitCodeAssert checks a service's exit code.
type ExitCodeAssert struct {
	Service string `yaml:"service"`
	Equals  int    `yaml:"equals"`
}

// EventuallyAssert polls an HTTP endpoint until expected status.
type EventuallyAssert struct {
	Timeout Duration   `yaml:"timeout"`
	HTTP    *HTTPCheck `yaml:"http"`
}

// HTTPCheck polls an HTTP endpoint.
type HTTPCheck struct {
	URL    string `yaml:"url"`
	Status int    `yaml:"status"`
}

// ---------------------------------------------------------------------------
// Duration
// ---------------------------------------------------------------------------

// Duration wraps time.Duration for YAML unmarshalling.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadTopology parses a faultbox.yaml file.
func LoadTopology(path string) (*TopologyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read topology %s: %w", path, err)
	}
	var cfg TopologyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse topology %s: %w", path, err)
	}
	migrateTopology(&cfg)
	if err := validateTopology(&cfg); err != nil {
		return nil, fmt.Errorf("validate topology %s: %w", path, err)
	}
	return &cfg, nil
}

// LoadSpec parses a spec.yaml file.
func LoadSpec(path string) (*SpecConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read spec %s: %w", path, err)
	}
	var cfg SpecConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse spec %s: %w", path, err)
	}
	if err := validateSpec(&cfg); err != nil {
		return nil, fmt.Errorf("validate spec %s: %w", path, err)
	}
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// Migration (legacy → new format)
// ---------------------------------------------------------------------------

// migrateTopology converts legacy fields to the new format.
func migrateTopology(cfg *TopologyConfig) {
	for name, svc := range cfg.Services {
		// Migrate port → interfaces.default
		if svc.Port > 0 && len(svc.Interfaces) == 0 {
			svc.Interfaces = map[string]InterfaceConfig{
				"default": {Protocol: "tcp", Port: svc.Port},
			}
			svc.Port = 0
		}

		// Migrate env → environment
		if len(svc.Env) > 0 && len(svc.Environment) == 0 {
			svc.Environment = svc.Env
			svc.Env = nil
		}

		// Migrate ready → healthcheck
		if svc.Ready != "" && svc.Healthcheck == nil {
			svc.Healthcheck = &HealthcheckConfig{
				Test:     svc.Ready,
				Interval: Duration{time.Second},
				Timeout:  Duration{10 * time.Second},
			}
			svc.Ready = ""
		}

		cfg.Services[name] = svc
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func validateTopology(cfg *TopologyConfig) error {
	if len(cfg.Services) == 0 {
		return fmt.Errorf("no services defined")
	}
	for name, svc := range cfg.Services {
		if svc.Binary == "" {
			return fmt.Errorf("service %q: binary is required", name)
		}
		if len(svc.Interfaces) == 0 {
			return fmt.Errorf("service %q: at least one interface is required (or set port for legacy mode)", name)
		}
		for ifName, iface := range svc.Interfaces {
			if iface.Port <= 0 {
				return fmt.Errorf("service %q interface %q: port is required", name, ifName)
			}
			if iface.Protocol == "" {
				return fmt.Errorf("service %q interface %q: protocol is required", name, ifName)
			}
		}
		for _, dep := range svc.DependsOn {
			if _, ok := cfg.Services[dep]; !ok {
				return fmt.Errorf("service %q: depends_on %q not found", name, dep)
			}
		}
	}
	return nil
}

func validateSpec(cfg *SpecConfig) error {
	if len(cfg.Traces) == 0 {
		return fmt.Errorf("no traces defined")
	}
	for name, trace := range cfg.Traces {
		for i, step := range trace.Steps {
			if step.Sleep == nil && step.Action == "" {
				return fmt.Errorf("trace %q step[%d]: invalid step (no action)", name, i)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Service Discovery Helpers
// ---------------------------------------------------------------------------

// ResolveServiceAddr returns "host:port" for a service's interface.
// For PoC, host is always "localhost".
func ResolveServiceAddr(cfg *TopologyConfig, service, iface string) (string, error) {
	svc, ok := cfg.Services[service]
	if !ok {
		return "", fmt.Errorf("service %q not found", service)
	}

	// If interface not specified, use the only one (or "default").
	if iface == "" {
		if len(svc.Interfaces) == 1 {
			for _, ic := range svc.Interfaces {
				return fmt.Sprintf("localhost:%d", ic.Port), nil
			}
		}
		if ic, ok := svc.Interfaces["default"]; ok {
			return fmt.Sprintf("localhost:%d", ic.Port), nil
		}
		return "", fmt.Errorf("service %q has multiple interfaces, specify which one", service)
	}

	ic, ok := svc.Interfaces[iface]
	if !ok {
		return "", fmt.Errorf("service %q interface %q not found", service, iface)
	}
	return fmt.Sprintf("localhost:%d", ic.Port), nil
}

// ResolveInterfaceProtocol returns the protocol for a service's interface.
func ResolveInterfaceProtocol(cfg *TopologyConfig, service, iface string) (string, error) {
	svc, ok := cfg.Services[service]
	if !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	if iface == "" {
		if len(svc.Interfaces) == 1 {
			for _, ic := range svc.Interfaces {
				return ic.Protocol, nil
			}
		}
		if ic, ok := svc.Interfaces["default"]; ok {
			return ic.Protocol, nil
		}
		return "", fmt.Errorf("service %q has multiple interfaces, specify which one", service)
	}
	ic, ok := svc.Interfaces[iface]
	if !ok {
		return "", fmt.Errorf("service %q interface %q not found", service, iface)
	}
	return ic.Protocol, nil
}

// ---------------------------------------------------------------------------
// Dependency ordering
// ---------------------------------------------------------------------------

// DependencyOrder returns service names in topological order (dependencies first).
func DependencyOrder(services map[string]ServiceConfig) ([]string, error) {
	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	var order []string

	var visit func(name string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("circular dependency involving %q", name)
		}
		visiting[name] = true
		svc := services[name]
		for _, dep := range svc.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for name := range services {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}
