// Package protocol defines the Protocol plugin interface and a global registry.
// Protocols provide step methods (HTTP get/post, Postgres query, Redis set, etc.)
// and healthchecks for service interfaces.
//
// Plugins self-register via init():
//
//	func init() { protocol.Register(&myProtocol{}) }
package protocol

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Protocol is the interface that all protocol plugins must implement.
type Protocol interface {
	// Name returns the protocol identifier used in interface() declarations.
	// Example: "http", "tcp", "postgres", "redis", "kafka".
	Name() string

	// Methods returns the step method names available on this protocol.
	// These become callable attributes on InterfaceRef in Starlark.
	// Example: ["get", "post", "put", "delete", "patch"] for HTTP.
	Methods() []string

	// Healthcheck checks if a service at the given address is ready.
	Healthcheck(ctx context.Context, addr string, timeout time.Duration) error

	// ExecuteStep runs a step method against a service.
	// The method parameter is one of the names returned by Methods().
	// kwargs contains Starlark keyword arguments (e.g., path, body, sql, key).
	// The response Body must be a JSON string (auto-decoded to .data in Starlark).
	ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error)
}

// StepResult is the response from a protocol step execution.
// Body is always a JSON string — the Starlark runtime auto-decodes it
// into a native dict/list on Response.data.
type StepResult struct {
	StatusCode int               // HTTP-style status (0 for non-HTTP protocols on success)
	Body       string            // JSON-encoded response data
	Success    bool              // true if the step completed without error
	Error      string            // error message if Success is false
	DurationMs int64             // step execution time in milliseconds
	Fields     map[string]string // optional extra fields for event emission
}

// registry holds all registered protocol plugins.
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Protocol)
)

// Register adds a protocol to the global registry.
// Panics if a protocol with the same name is already registered.
// Call this from init() in protocol implementation files.
func Register(p Protocol) {
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("protocol %q already registered", name))
	}
	registry[name] = p
}

// Get returns a registered protocol by name, or nil and false if not found.
func Get(name string) (Protocol, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// MustGet returns a registered protocol by name, or panics if not found.
func MustGet(name string) Protocol {
	p, ok := Get(name)
	if !ok {
		panic(fmt.Sprintf("protocol %q not registered", name))
	}
	return p
}

// Names returns all registered protocol names (sorted order not guaranteed).
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
