package star

import "fmt"

// Called before a native binary may exec. Registration is observation, not a
// change to the program's client IDs or Kafka protocol. Restricted/unsupported
// procfs remains explicit unknown evidence; it never guesses another service.
func (rt *Runtime) registerProcess(service string, pid int) (func(), error) {
	if rt.connections == nil {
		return func() {}, nil
	}
	cleanup, err := rt.connections.Register(service, pid)
	if err != nil {
		rt.events.recordOnly("process_ownership_unavailable", service, map[string]string{"pid": fmt.Sprint(pid), "error": err.Error()})
		rt.log.Warn("process socket ownership unavailable", "service", service, "pid", pid, "error", err.Error())
		return func() {}, nil
	}
	rt.events.recordOnly("service_process_registered", service, map[string]string{"pid": fmt.Sprint(pid)})
	return cleanup, nil
}
