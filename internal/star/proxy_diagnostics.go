package star

import "fmt"

// Detect empty protocol-fault scopes, including rules on an interface that
// the SUT bypassed. A successful body alone is not evidence of fault tolerance.
func protocolFaultDiagnostics(events []Event) []Diagnostic {
	var out []Diagnostic
	for i, applied := range events {
		if applied.Type != "proxy_fault_applied" {
			continue
		}
		hits := 0
		for _, ev := range events[i+1:] {
			if ev.Service != applied.Service || ev.Fields["interface"] != applied.Fields["interface"] {
				continue
			}
			if ev.Type == "proxy_fault_removed" {
				break
			}
			if ev.Type == "proxy" && ev.Fields["action"] != "forward" && ev.Fields["action"] != "" {
				hits++
			}
		}
		if hits == 0 {
			out = append(out, Diagnostic{Level: "warning", Code: "FAULT_NOT_FIRED", Service: applied.Service,
				Message:    fmt.Sprintf("%s protocol fault scope on %s.%s never fired", applied.Fields["protocol"], applied.Service, applied.Fields["interface"]),
				Suggestion: "Check the rule match and route SUT traffic through the interface proxy address."})
		}
	}
	return out
}
