package star

import (
	"context"
	"errors"
	"github.com/faultbox/Faultbox/internal/engine"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPortReleaseWaitAndForeignListenerAreSafe(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	rt := New(testLogger())
	rt.shutdownTimeout = 60 * time.Millisecond
	svc := &ServiceDef{Name: "leaked", Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: "tcp", Port: port}}}
	rt.services[svc.Name] = svc
	done := make(chan *engine.Result, 1)
	done <- &engine.Result{}
	rt.stopOneService(svc.Name, &runningSession{done: done})
	if !rt.ShutdownFailed() {
		t.Fatal("failed port cleanup did not poison runtime")
	}
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("foreign listener was killed", err)
	}
	conn.Close()
	if got := rt.RunTest(context.Background(), "anything"); !strings.Contains(got.Reason, "TEARDOWN_FAILED") {
		t.Fatal(got)
	}
}
func TestProcessWaitFailurePoisonsRuntime(t *testing.T) {
	rt := New(testLogger())
	done := make(chan *engine.Result, 1)
	done <- &engine.Result{Error: engine.ErrProcessWait}
	rt.stopOneService("unknown-status", &runningSession{done: done})
	if !rt.ShutdownFailed() {
		t.Fatal("unknown process exit accepted")
	}
}
func TestPortsEventuallyRelease(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	rt := New(testLogger())
	svc := &ServiceDef{Name: "slow-stop", Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: "tcp", Port: port}}}
	timer := time.AfterFunc(30*time.Millisecond, func() { ln.Close() })
	defer timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rt.waitServicePortsReleased(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := rt.explainPortConflict(errors.New("other")); err.Error() != "other" {
		t.Fatal(err)
	}
}
