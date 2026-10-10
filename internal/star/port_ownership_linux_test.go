package star

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestPortConflictAttributesOnlyRegisteredIncarnation(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	rt.currentTestName = "test_previous"
	cleanup, err := rt.registerProcess("previous", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	svc := &ServiceDef{Name: "next", Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: "tcp", Port: ln.Addr().(*net.TCPAddr).Port}}}
	rt.currentTestName = "test_next"
	err = rt.explainPortConflict(checkFixedServicePorts(svc))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint("pid=", os.Getpid())) || !strings.Contains(err.Error(), `test="test_previous"`) {
		t.Fatal(err)
	}
	origin := rt.processOrigins[os.Getpid()]
	origin.start = "previous-incarnation"
	rt.processOrigins[os.Getpid()] = origin
	err = rt.explainPortConflict(checkFixedServicePorts(svc))
	if strings.Contains(err.Error(), `test="test_previous"`) {
		t.Fatal("PID reuse falsely attributed", err)
	}
}
