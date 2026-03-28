// Package star provides a Starlark runtime for Faultbox configuration and tests.
package star

import (
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

// ---------------------------------------------------------------------------
// ServiceDef — returned by service() builtin, used in Starlark scripts
// ---------------------------------------------------------------------------

// ServiceDef is the Starlark representation of a service declaration.
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
	Healthcheck *HealthcheckDef
	rt         *Runtime // set by runtime after registration
}

// IsContainer returns true if this service uses a container image.
func (s *ServiceDef) IsContainer() bool {
	return s.Image != "" || s.Build != ""
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
	case "get", "post", "put", "delete", "patch":
		return &StepMethod{Ref: r, Method: name}, nil
	case "send":
		return &StepMethod{Ref: r, Method: "send"}, nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("interface_ref has no .%s attribute", name))
}

func (r *InterfaceRef) AttrNames() []string {
	if r.Interface.Protocol == "tcp" {
		return []string{"addr", "host", "port", "send"}
	}
	return []string{"addr", "host", "port", "get", "post", "put", "delete", "patch"}
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
	return []string{"status", "body", "ok", "error", "duration_ms"}
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
}

var _ starlark.Value = (*FaultDef)(nil)

func (f *FaultDef) String() string {
	if f.Action == "delay" {
		return fmt.Sprintf("<fault delay=%s prob=%.0f%%>", f.Delay, f.Probability*100)
	}
	return fmt.Sprintf("<fault deny=%s prob=%.0f%%>", f.Errno, f.Probability*100)
}
func (f *FaultDef) Type() string           { return "fault" }
func (f *FaultDef) Freeze()                {}
func (f *FaultDef) Truth() starlark.Bool   { return true }
func (f *FaultDef) Hash() (uint32, error)  { return 0, fmt.Errorf("unhashable: fault") }

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
