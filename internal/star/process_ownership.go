package star

import (
	"fmt"
	"github.com/faultbox/Faultbox/internal/portowner"
)

// Called before a native binary may exec. Registration is observation, not a
// change to the program's client IDs or Kafka protocol. Restricted/unsupported
// procfs remains explicit unknown evidence; it never guesses another service.
func (rt *Runtime) registerProcess(service string, pid int) (func(), error) {
	return rt.registerProcessForTest(service, pid, rt.currentTestName)
}

func (rt *Runtime) registerProcessForTest(service string, pid int, testName string) (func(), error) {
	if rt.connections == nil {
		return func() {}, nil
	}
	rt.processOriginMu.Lock()
	if rt.processOrigins == nil {
		rt.processOrigins = make(map[int]processOrigin)
	}
	rt.processOrigins[pid] = processOrigin{start: portowner.Start(pid), service: service, test: testName}
	rt.processOriginMu.Unlock()
	cleanup, err := rt.connections.Register(service, pid)
	if err != nil {
		rt.events.recordOnly("process_ownership_unavailable", service, map[string]string{"pid": fmt.Sprint(pid), "error": err.Error()})
		rt.log.Warn("process socket ownership unavailable", "service", service, "pid", pid, "error", err.Error())
		return func() {}, nil
	}
	rt.events.recordOnly("service_process_registered", service, map[string]string{"pid": fmt.Sprint(pid)})
	return cleanup, nil
}
