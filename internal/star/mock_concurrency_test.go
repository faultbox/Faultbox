package star

import (
	"github.com/faultbox/Faultbox/internal/protocol"
	"go.starlark.net/starlark"
	"testing"
	"time"
)

func TestDynamicMocksDoNotShareRuntimeLock(t *testing.T) {
	rt := New(testLogger())
	for _, name := range []string{"slow", "fast"} {
		m := &MockConfig{}
		m.resetState()
		rt.services[name] = &ServiceDef{Name: name, Mock: m}
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	slow := rt.dynamicHandlerBridge("slow", "main", "/slow", starlark.NewBuiltin("slow", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		close(started)
		<-release
		return &MockResponseValue{static: &protocol.MockResponse{Status: 200}}, nil
	}))
	fast := rt.dynamicHandlerBridge("fast", "main", "/fast", starlark.NewBuiltin("fast", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		return &MockResponseValue{static: &protocol.MockResponse{Status: 200}}, nil
	}))
	go slow(protocol.MockRequest{})
	<-started
	done := make(chan error, 1)
	go func() { _, err := fast(protocol.MockRequest{}); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated mock blocked behind slow handler")
	}
	acquired := make(chan struct{})
	go func() { rt.mu.Lock(); rt.mu.Unlock(); close(acquired) }()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("runtime metadata lock held during handler")
	}
}
