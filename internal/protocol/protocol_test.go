package protocol

import (
	"context"
	"testing"
	"time"
)

// testProtocol is a minimal Protocol for testing the registry.
type testProtocol struct {
	name    string
	methods []string
}

func (p *testProtocol) Name() string      { return p.name }
func (p *testProtocol) Methods() []string  { return p.methods }
func (p *testProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	return nil
}
func (p *testProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	return &StepResult{Body: `{"ok":true}`, Success: true}, nil
}

func TestRegisterAndGet(t *testing.T) {
	// Reset registry for isolated test.
	registryMu.Lock()
	old := registry
	registry = make(map[string]Protocol)
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = old
		registryMu.Unlock()
	}()

	p := &testProtocol{name: "test-proto", methods: []string{"ping"}}
	Register(p)

	got, ok := Get("test-proto")
	if !ok {
		t.Fatal("expected to find test-proto")
	}
	if got.Name() != "test-proto" {
		t.Fatalf("Name() = %q, want test-proto", got.Name())
	}

	_, ok = Get("nonexistent")
	if ok {
		t.Fatal("expected nonexistent to not be found")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	registryMu.Lock()
	old := registry
	registry = make(map[string]Protocol)
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = old
		registryMu.Unlock()
	}()

	Register(&testProtocol{name: "dup"})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register(&testProtocol{name: "dup"})
}

func TestMustGetPanics(t *testing.T) {
	registryMu.Lock()
	old := registry
	registry = make(map[string]Protocol)
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = old
		registryMu.Unlock()
	}()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustGet")
		}
	}()
	MustGet("missing")
}

func TestNames(t *testing.T) {
	registryMu.Lock()
	old := registry
	registry = make(map[string]Protocol)
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = old
		registryMu.Unlock()
	}()

	Register(&testProtocol{name: "alpha"})
	Register(&testProtocol{name: "beta"})

	names := Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
}
