package star

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// HostEphemeralPortRange returns the actual host setting, never a guessed
// platform default. A check executed inside Lima therefore uses Lima's range.
func HostEphemeralPortRange() (low, high int, err error) {
	return hostEphemeralPortRange()
}

func parseEphemeralPortRange(value string) (low, high int, err error) {
	fields := strings.Fields(value)
	if len(fields) == 2 {
		low, err = strconv.Atoi(fields[0])
		if err == nil {
			high, err = strconv.Atoi(fields[1])
			if err == nil && low > 0 && low <= high && high <= 65535 {
				return low, high, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("invalid host ephemeral port range %q", value)
}

func fixedHostPorts(svc *ServiceDef) []int {
	if svc.IsRemote() {
		return nil
	}
	ports := map[int]bool{}
	for _, iface := range svc.Interfaces {
		port := iface.Port
		if svc.IsContainer() {
			port = svc.Ports[iface.Port] // zero means Docker publishes automatically.
		}
		if port > 0 {
			ports[port] = true
		}
	}
	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Ints(result)
	return result
}

// FixedPortDiagnostics inspects only listeners the spec fixes on this host.
// Remote endpoints and auto-published Docker ports cannot collide through a
// spec's fixed binding, so neither produces this warning.
func (rt *Runtime) FixedPortDiagnostics(low, high int) []Diagnostic {
	if low <= 0 || high < low || high > 65535 {
		return nil
	}
	var result []Diagnostic
	services := rt.Services()
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	for _, svc := range services {
		for _, port := range fixedHostPorts(svc) {
			if port >= low && port <= high {
				result = append(result, Diagnostic{
					Level: "warning", Code: "PORT_IN_EPHEMERAL_RANGE",
					Message:    fmt.Sprintf("service %q fixes host port %d inside this host's ephemeral range %d-%d", svc.Name, port, low, high),
					Suggestion: "Choose a fixed listener port outside this host's ephemeral range, or let Docker publish automatically. Run check on the execution host (inside Lima when applicable).",
				})
			}
		}
	}
	return result
}
