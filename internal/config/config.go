// Package config provides YAML parsing for Faultbox topology and spec files.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// TopologyConfig is parsed from faultbox.yaml — describes the system topology.
type TopologyConfig struct {
	Version  string                   `yaml:"version"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// ServiceConfig describes one service in the topology.
type ServiceConfig struct {
	Binary    string            `yaml:"binary"`
	Args      []string          `yaml:"args"`
	Port      int               `yaml:"port"`
	Env       map[string]string `yaml:"env"`
	DependsOn []string          `yaml:"depends_on"`
	Ready     string            `yaml:"ready"`
}

// SpecConfig is parsed from spec.yaml — describes test traces.
type SpecConfig struct {
	Version string                 `yaml:"version"`
	System  string                 `yaml:"system"`
	Traces  map[string]TraceConfig `yaml:"traces"`
}

// TraceConfig describes one test trace.
type TraceConfig struct {
	Description string                       `yaml:"description"`
	Faults      map[string][]string          `yaml:"faults"`
	Assert      []AssertConfig               `yaml:"assert"`
	Timeout     Duration                     `yaml:"timeout"`
}

// AssertConfig describes one assertion in a trace.
// For PoC: supports exit_code checks and eventually(http.get).
type AssertConfig struct {
	// ExitCode checks: "api.exit_code == 0"
	ExitCode *ExitCodeAssert `yaml:"exit_code"`
	// Eventually checks: poll until condition is met
	Eventually *EventuallyAssert `yaml:"eventually"`
}

// ExitCodeAssert checks a service's exit code.
type ExitCodeAssert struct {
	Service string `yaml:"service"`
	Equals  int    `yaml:"equals"`
}

// EventuallyAssert polls an HTTP endpoint until expected status.
type EventuallyAssert struct {
	Timeout Duration `yaml:"timeout"`
	HTTP    *HTTPCheck `yaml:"http"`
}

// HTTPCheck polls an HTTP endpoint.
type HTTPCheck struct {
	URL    string `yaml:"url"`
	Status int    `yaml:"status"`
}

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

func validateTopology(cfg *TopologyConfig) error {
	if len(cfg.Services) == 0 {
		return fmt.Errorf("no services defined")
	}
	for name, svc := range cfg.Services {
		if svc.Binary == "" {
			return fmt.Errorf("service %q: binary is required", name)
		}
		// Validate depends_on references exist.
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
	return nil
}

// DependencyOrder returns service names in topological order (dependencies first).
// Returns an error if there are circular dependencies.
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
