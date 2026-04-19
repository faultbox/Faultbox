// Package star provides a Starlark runtime for Faultbox configuration and tests.
package star

import (
	jsonPkg "encoding/json"
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// ---------------------------------------------------------------------------
// ServiceDef — returned by service() builtin, used in Starlark scripts
// ---------------------------------------------------------------------------

// ServiceDef is the Starlark representation of a service declaration.
// OpDef defines a named operation — a group of syscalls + optional path filter.
// Created by the op() builtin, stored in ServiceDef.Ops.
type OpDef struct {
	Name     string   // set when attached to service
	Syscalls []string // e.g., ["write", "fsync"]
	Path     string   // optional glob (e.g., "/tmp/*.wal")
}

var _ starlark.Value = (*OpDef)(nil)

func (o *OpDef) String() string        { return fmt.Sprintf("<op %s syscalls=%v>", o.Name, o.Syscalls) }
func (o *OpDef) Type() string           { return "op" }
func (o *OpDef) Freeze()                {}
func (o *OpDef) Truth() starlark.Bool   { return true }
func (o *OpDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: op") }

// ObserveConfig describes an event source attached to a service.
type ObserveConfig struct {
	SourceName  string            // "stdout", "topic", "wal_stream", "tail", "poll"
	DecoderName string            // "json", "logfmt", "regex"
	Params      map[string]string // source-specific params
}

type ServiceDef struct {
	Name       string
	Binary     string            // local binary path (binary mode)
	Image      string            // container image reference (container mode)
	Build      string            // Dockerfile context path (container mode)
	Args       []string
	Interfaces map[string]*InterfaceDef
	Env        map[string]string
	DependsOn  []string
	Volumes    map[string]string // host:container volume mounts (container mode)
	Ports      map[int]int       // container_port → host_port override (0 = Docker picks)
	Healthcheck *HealthcheckDef
	Observe    []ObserveConfig   // event sources to attach
	Ops        map[string]*OpDef // named operations (e.g., "persist" → write+fsync+path)

	// Container reuse lifecycle (RFC-015).
	Reuse bool               // keep container alive between tests
	Seed  starlark.Callable  // initialize service state (runs once after healthcheck)
	Reset starlark.Callable  // re-initialize between tests (defaults to Seed if nil)

	// Mock service support (RFC-017). When non-nil, this ServiceDef is a
	// mock: the runtime starts an in-process protocol handler instead of
	// launching a binary or container.
	Mock *MockConfig

	rt         *Runtime // set by runtime after registration
}

// IsContainer returns true if this service uses a container image.
func (s *ServiceDef) IsContainer() bool {
	return s.Image != "" || s.Build != ""
}

// IsMock returns true if this service is a mock (RFC-017).
func (s *ServiceDef) IsMock() bool {
	return s.Mock != nil
}

var _ starlark.Value = (*ServiceDef)(nil)
var _ starlark.HasAttrs = (*ServiceDef)(nil)

func (s *ServiceDef) String() string        { return fmt.Sprintf("<service %s>", s.Name) }
func (s *ServiceDef) Type() string           { return "service" }
func (s *ServiceDef) Freeze()                {}
func (s *ServiceDef) Truth() starlark.Bool   { return true }
func (s *ServiceDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: service") }

func (s *ServiceDef) Attr(name string) (starlark.Value, error) {
	// Direct interface access: service.main, service.public, etc.
	if iface, ok := s.Interfaces[name]; ok {
		return &InterfaceRef{Service: s, Interface: iface, runtime: s.rt}, nil
	}
	switch name {
	case "name":
		return starlark.String(s.Name), nil
	case "get", "post", "put", "delete", "patch", "send":
		// Shorthand: api.post(...) when service has a single interface.
		iface, err := s.DefaultInterface()
		if err != nil {
			return nil, err
		}
		ref := &InterfaceRef{Service: s, Interface: iface, runtime: s.rt}
		return &StepMethod{Ref: ref, Method: name}, nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("service has no .%s attribute", name))
}

func (s *ServiceDef) AttrNames() []string {
	names := []string{"name"}
	for k := range s.Interfaces {
		names = append(names, k)
	}
	return names
}

// DefaultInterface returns the single interface or "default" interface.
func (s *ServiceDef) DefaultInterface() (*InterfaceDef, error) {
	if len(s.Interfaces) == 1 {
		for _, iface := range s.Interfaces {
			return iface, nil
		}
	}
	if iface, ok := s.Interfaces["default"]; ok {
		return iface, nil
	}
	return nil, fmt.Errorf("service %q has multiple interfaces, specify which one", s.Name)
}

// ---------------------------------------------------------------------------
// InterfaceDef — returned by interface() builtin
// ---------------------------------------------------------------------------

type InterfaceDef struct {
	Name     string
	Protocol string
	Port     int
	HostPort int    // actual host-mapped port (container mode, 0 = same as Port)
	Spec     string // path to protocol spec file (swagger, proto, etc.)
}

var _ starlark.Value = (*InterfaceDef)(nil)

func (i *InterfaceDef) String() string        { return fmt.Sprintf("<interface %s %s:%d>", i.Name, i.Protocol, i.Port) }
func (i *InterfaceDef) Type() string           { return "interface" }
func (i *InterfaceDef) Freeze()                {}
func (i *InterfaceDef) Truth() starlark.Bool   { return true }
func (i *InterfaceDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: interface") }

// ---------------------------------------------------------------------------
// InterfaceRef — service.main, exposes .addr, .post(), .get(), .send()
// ---------------------------------------------------------------------------

// InterfaceRef binds a service to a specific interface for step execution.
// It is also callable for steps: api.public.post(...) calls through runtime.
type InterfaceRef struct {
	Service   *ServiceDef
	Interface *InterfaceDef
	runtime   *Runtime // set when runtime is available
}

var _ starlark.Value = (*InterfaceRef)(nil)
var _ starlark.HasAttrs = (*InterfaceRef)(nil)

func (r *InterfaceRef) String() string        { return fmt.Sprintf("<interface_ref %s.%s>", r.Service.Name, r.Interface.Name) }
func (r *InterfaceRef) Type() string           { return "interface_ref" }
func (r *InterfaceRef) Freeze()                {}
func (r *InterfaceRef) Truth() starlark.Bool   { return true }
func (r *InterfaceRef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: interface_ref") }

func (r *InterfaceRef) Attr(name string) (starlark.Value, error) {
	switch name {
	case "addr":
		port := r.Interface.Port
		if r.Interface.HostPort > 0 {
			port = r.Interface.HostPort
		}
		return starlark.String(fmt.Sprintf("localhost:%d", port)), nil
	case "internal_addr":
		// For container-to-container: use service name as hostname.
		// For binary mode: same as addr (localhost).
		if r.Service.IsContainer() {
			return starlark.String(fmt.Sprintf("%s:%d", r.Service.Name, r.Interface.Port)), nil
		}
		return starlark.String(fmt.Sprintf("localhost:%d", r.Interface.Port)), nil
	case "host":
		return starlark.String("localhost"), nil
	case "port":
		return starlark.MakeInt(r.Interface.Port), nil
	}

	// Check if this is a step method from the protocol plugin.
	if p, ok := protocol.Get(r.Interface.Protocol); ok {
		for _, m := range p.Methods() {
			if m == name {
				return &StepMethod{Ref: r, Method: name}, nil
			}
		}
	}

	return nil, starlark.NoSuchAttrError(fmt.Sprintf("interface_ref has no .%s attribute", name))
}

func (r *InterfaceRef) AttrNames() []string {
	base := []string{"addr", "internal_addr", "host", "port"}
	if p, ok := protocol.Get(r.Interface.Protocol); ok {
		base = append(base, p.Methods()...)
	}
	return base
}

// ---------------------------------------------------------------------------
// StepMethod — callable step like api.public.post(path="/health")
// ---------------------------------------------------------------------------

type StepMethod struct {
	Ref    *InterfaceRef
	Method string
}

var _ starlark.Callable = (*StepMethod)(nil)

func (m *StepMethod) String() string  { return fmt.Sprintf("<step %s.%s.%s>", m.Ref.Service.Name, m.Ref.Interface.Name, m.Method) }
func (m *StepMethod) Type() string    { return "step_method" }
func (m *StepMethod) Freeze()         {}
func (m *StepMethod) Truth() starlark.Bool { return true }
func (m *StepMethod) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: step_method") }
func (m *StepMethod) Name() string    { return m.Method }

func (m *StepMethod) CallInternal(thread *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	rt := m.Ref.runtime
	if rt == nil {
		return nil, fmt.Errorf("step %s: runtime not available (services not started)", m.String())
	}
	return rt.executeStep(thread, m.Ref, m.Method, args, kwargs)
}

// ---------------------------------------------------------------------------
// Response — wraps step result for Starlark access
// ---------------------------------------------------------------------------

type Response struct {
	Status     int
	Body       string
	DurationMs int64
	Ok         bool
	Error      string
}

var _ starlark.Value = (*Response)(nil)
var _ starlark.HasAttrs = (*Response)(nil)

func (r *Response) String() string {
	if r.Ok {
		return fmt.Sprintf("<response status=%d>", r.Status)
	}
	return fmt.Sprintf("<response error=%q>", r.Error)
}
func (r *Response) Type() string           { return "response" }
func (r *Response) Freeze()                {}
func (r *Response) Truth() starlark.Bool   { return starlark.Bool(r.Ok) }
func (r *Response) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: response") }

func (r *Response) Attr(name string) (starlark.Value, error) {
	switch name {
	case "status":
		return starlark.MakeInt(r.Status), nil
	case "body":
		return starlark.String(r.Body), nil
	case "data":
		// Auto-decode JSON body into native Starlark dict/list.
		if decoded := jsonToStarlark(r.Body); decoded != nil {
			return decoded, nil
		}
		// Fallback: return body as string if not valid JSON.
		return starlark.String(r.Body), nil
	case "ok":
		return starlark.Bool(r.Ok), nil
	case "error":
		return starlark.String(r.Error), nil
	case "duration_ms":
		return starlark.MakeInt64(r.DurationMs), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("response has no .%s attribute", name))
}

func (r *Response) AttrNames() []string {
	return []string{"status", "body", "data", "ok", "error", "duration_ms"}
}

// ---------------------------------------------------------------------------
// HealthcheckDef
// ---------------------------------------------------------------------------

type HealthcheckDef struct {
	Test    string        // "tcp://host:port" or "http://host/path"
	Timeout time.Duration
}

var _ starlark.Value = (*HealthcheckDef)(nil)

func (h *HealthcheckDef) String() string        { return fmt.Sprintf("<healthcheck %s>", h.Test) }
func (h *HealthcheckDef) Type() string           { return "healthcheck" }
func (h *HealthcheckDef) Freeze()                {}
func (h *HealthcheckDef) Truth() starlark.Bool   { return true }
func (h *HealthcheckDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: healthcheck") }

// ---------------------------------------------------------------------------
// FaultDef — returned by delay() / deny() builtins
// ---------------------------------------------------------------------------

type FaultDef struct {
	Action      string // "delay" or "deny"
	Delay       time.Duration
	Errno       string
	Probability float64
	Label       string // optional human-readable label (e.g., "WAL write")
	Op          string // operation name (set when expanded from ops=)
	PathGlob    string // path glob (set when expanded from ops=)
}

// ProxyFaultDef describes a protocol-level fault rule.
// Created by response(), error(), delay(), drop() builtins when used
// with fault(interface_ref, ...).
type ProxyFaultDef struct {
	// Match criteria (glob patterns):
	Method  string // HTTP method, gRPC method, Redis command
	Path    string // HTTP path pattern
	Query   string // SQL query pattern
	Key     string // Redis key pattern
	Topic   string // Kafka/NATS topic
	Command string // Redis command name

	// Action:
	Action string // "respond", "error", "delay", "drop", "duplicate"

	// Parameters:
	Status      int
	Body        string
	Error       string
	Delay       time.Duration
	Probability float64
}

var _ starlark.Value = (*ProxyFaultDef)(nil)

func (f *ProxyFaultDef) String() string {
	return fmt.Sprintf("<proxy_fault %s>", f.Action)
}
func (f *ProxyFaultDef) Type() string           { return "proxy_fault" }
func (f *ProxyFaultDef) Freeze()                {}
func (f *ProxyFaultDef) Truth() starlark.Bool   { return true }
func (f *ProxyFaultDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: proxy_fault") }

var _ starlark.Value = (*FaultDef)(nil)

func (f *FaultDef) String() string {
	var s string
	if f.Action == "delay" {
		s = fmt.Sprintf("<fault delay=%s prob=%.0f%%", f.Delay, f.Probability*100)
	} else {
		s = fmt.Sprintf("<fault deny=%s prob=%.0f%%", f.Errno, f.Probability*100)
	}
	if f.Label != "" {
		s += fmt.Sprintf(" label=%q", f.Label)
	}
	return s + ">"
}
func (f *FaultDef) Type() string           { return "fault" }
func (f *FaultDef) Freeze()                {}
func (f *FaultDef) Truth() starlark.Bool   { return true }
func (f *FaultDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: fault") }

// ---------------------------------------------------------------------------
// FaultAssumptionDef — named, reusable fault configuration
// ---------------------------------------------------------------------------

// FaultAssumptionRule is a single syscall-level fault targeting a service.
type FaultAssumptionRule struct {
	Target  *ServiceDef
	Syscall string    // resolved syscall name (after family expansion)
	Fault   *FaultDef
}

// FaultAssumptionProxyRule is a single protocol-level fault targeting an interface.
type FaultAssumptionProxyRule struct {
	Target     *InterfaceRef
	ProxyFault *ProxyFaultDef
}

// FaultAssumptionDef is a first-class Starlark value representing a named,
// reusable fault configuration. Created by fault_assumption() builtin.
type FaultAssumptionDef struct {
	Name        string
	Description string
	Rules       []FaultAssumptionRule
	ProxyRules  []FaultAssumptionProxyRule
	Monitors    []*MonitorDef
}

var _ starlark.Value = (*FaultAssumptionDef)(nil)

func (a *FaultAssumptionDef) String() string {
	parts := []string{}
	for _, r := range a.Rules {
		parts = append(parts, fmt.Sprintf("%s.%s=%s", r.Target.Name, r.Syscall, r.Fault.Action))
	}
	for _, r := range a.ProxyRules {
		parts = append(parts, fmt.Sprintf("%s.%s=%s", r.Target.Service.Name, r.Target.Interface.Name, r.ProxyFault.Action))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("<fault_assumption %s>", a.Name)
	}
	return fmt.Sprintf("<fault_assumption %s %s>", a.Name, strings.Join(parts, " "))
}
func (a *FaultAssumptionDef) Type() string           { return "fault_assumption" }
func (a *FaultAssumptionDef) Freeze()                {}
func (a *FaultAssumptionDef) Truth() starlark.Bool   { return true }
func (a *FaultAssumptionDef) Hash() (uint32, error) {
	// Hash by name — names are unique in the registry.
	var h uint32
	for _, c := range a.Name {
		h = h*31 + uint32(c)
	}
	return h, nil
}

// ---------------------------------------------------------------------------
// FaultScenarioDef — composed scenario + faults + oracle
// ---------------------------------------------------------------------------

// FaultScenarioDef is a test definition that composes a scenario probe
// with fault assumptions, monitors, and an expect oracle.
// Created by fault_scenario() builtin, registered as test_<name>.
type FaultScenarioDef struct {
	Name     string
	Scenario starlark.Callable
	Faults   []*FaultAssumptionDef
	Expect   starlark.Callable // may be nil (smoke test)
	Monitors []*MonitorDef
	Timeout  time.Duration
	Matrix   *MatrixInfo // non-nil if generated by fault_matrix()
}

// MatrixInfo tracks which matrix cell a fault_scenario belongs to.
type MatrixInfo struct {
	ScenarioName string
	FaultName    string
}

// ---------------------------------------------------------------------------
// MonitorDef — first-class monitor value
// ---------------------------------------------------------------------------

// MonitorDef is a first-class Starlark value representing a monitor.
// Created by monitor() builtin, can be stored in variables and passed
// to fault_assumption(monitors=) and fault_scenario(monitors=).
type MonitorDef struct {
	Callback starlark.Callable
	Filters  []EventFilter
}

// EventFilter is a key-value pair for filtering events.
// Exported so MonitorDef can reference it from types.go.
type EventFilter struct {
	Key   string // "service", "syscall", "path", "decision", "type"
	Value string // value to match (supports trailing * for glob)
}

var _ starlark.Value = (*MonitorDef)(nil)

func (m *MonitorDef) String() string {
	var parts []string
	for _, f := range m.Filters {
		parts = append(parts, f.Key+"="+f.Value)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("<monitor %s>", m.Callback.Name())
	}
	return fmt.Sprintf("<monitor %s %s>", m.Callback.Name(), strings.Join(parts, " "))
}
func (m *MonitorDef) Type() string           { return "monitor" }
func (m *MonitorDef) Freeze()                {}
func (m *MonitorDef) Truth() starlark.Bool   { return true }
func (m *MonitorDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: monitor") }

// ---------------------------------------------------------------------------
// StarlarkEvent — wraps Event for lambda predicate access
// ---------------------------------------------------------------------------

// StarlarkEvent wraps an Event for use in Starlark lambda predicates.
// Provides .service, .type, .data (auto-decoded JSON), .fields, .seq.
type StarlarkEvent struct {
	ev    Event
	first *StarlarkEvent // for assert_before: the matched first event
}

var _ starlark.Value = (*StarlarkEvent)(nil)
var _ starlark.HasAttrs = (*StarlarkEvent)(nil)

func (e *StarlarkEvent) String() string {
	return fmt.Sprintf("<event #%d %s %s>", e.ev.Seq, e.ev.Service, e.ev.EventType)
}
func (e *StarlarkEvent) Type() string           { return "event" }
func (e *StarlarkEvent) Freeze()                {}
func (e *StarlarkEvent) Truth() starlark.Bool   { return true }
func (e *StarlarkEvent) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: event") }

func (e *StarlarkEvent) Attr(name string) (starlark.Value, error) {
	switch name {
	case "seq":
		return starlark.MakeInt64(e.ev.Seq), nil
	case "service":
		return starlark.String(e.ev.Service), nil
	case "type":
		return starlark.String(e.ev.Type), nil
	case "event_type":
		return starlark.String(e.ev.EventType), nil
	case "data":
		// Auto-decode JSON from the "data" field if present,
		// otherwise return all fields as a dict.
		return e.fieldsAsData(), nil
	case "fields":
		return e.fieldsDict(), nil
	case "first":
		if e.first != nil {
			return e.first, nil
		}
		return starlark.None, nil
	}
	// Direct access to event fields — returns empty string if not present
	// (so lambda predicates like e.path don't error on events without that field).
	if v, ok := e.ev.Fields[name]; ok {
		return starlark.String(v), nil
	}
	return starlark.String(""), nil
}

func (e *StarlarkEvent) AttrNames() []string {
	return []string{"seq", "service", "type", "event_type", "data", "fields", "first", "op"}
}

// fieldsDict returns all event fields as a Starlark dict.
func (e *StarlarkEvent) fieldsDict() *starlark.Dict {
	d := starlark.NewDict(len(e.ev.Fields))
	for k, v := range e.ev.Fields {
		d.SetKey(starlark.String(k), starlark.String(v))
	}
	return d
}

// fieldsAsData returns event fields as a Starlark dict.
// If a "data" field exists containing JSON, it's auto-decoded.
// Otherwise returns all fields as a dict.
func (e *StarlarkEvent) fieldsAsData() starlark.Value {
	if jsonStr, ok := e.ev.Fields["data"]; ok && len(jsonStr) > 0 {
		if decoded := jsonToStarlark(jsonStr); decoded != nil {
			return decoded
		}
	}
	// Fallback: return all fields as a dict.
	return e.fieldsDict()
}

// jsonToStarlark attempts to decode a JSON string into Starlark values.
// Returns nil if parsing fails.
func jsonToStarlark(s string) starlark.Value {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return nil
	}
	// Try to parse as JSON using Go's json package, then convert.
	var raw any
	if err := jsonUnmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	return goToStarlark(raw)
}

// goToStarlark converts a Go value (from json.Unmarshal) to Starlark.
func goToStarlark(v any) starlark.Value {
	switch val := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(val)
	case float64:
		if val == float64(int64(val)) {
			return starlark.MakeInt64(int64(val))
		}
		return starlark.Float(val)
	case string:
		return starlark.String(val)
	case []any:
		items := make([]starlark.Value, len(val))
		for i, item := range val {
			items[i] = goToStarlark(item)
		}
		return starlark.NewList(items)
	case map[string]any:
		d := starlark.NewDict(len(val))
		for k, v := range val {
			d.SetKey(starlark.String(k), goToStarlark(v))
		}
		return d
	default:
		return starlark.String(fmt.Sprint(v))
	}
}

// jsonUnmarshal is a thin wrapper for encoding/json.Unmarshal.
func jsonUnmarshal(data []byte, v any) error {
	return jsonPkg.Unmarshal(data, v)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func starStringArg(kwargs []starlark.Tuple, name, defaultVal string) string {
	for _, kv := range kwargs {
		if string(kv[0].(starlark.String)) == name {
			return string(kv[1].(starlark.String))
		}
	}
	return defaultVal
}

func starStringMapArg(kwargs []starlark.Tuple, name string) map[string]string {
	for _, kv := range kwargs {
		if string(kv[0].(starlark.String)) == name {
			dict, ok := kv[1].(*starlark.Dict)
			if !ok {
				return nil
			}
			m := make(map[string]string)
			for _, item := range dict.Items() {
				k, _ := starlark.AsString(item[0])
				v, _ := starlark.AsString(item[1])
				m[k] = v
			}
			return m
		}
	}
	return nil
}

func starKwarg(kwargs []starlark.Tuple, name string) (starlark.Value, bool) {
	for _, kv := range kwargs {
		if string(kv[0].(starlark.String)) == name {
			return kv[1], true
		}
	}
	return nil, false
}

func parseStarDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	return time.ParseDuration(s)
}
