// Package generate analyzes Faultbox topology and produces failure scenario
// mutations from registered happy-path scenarios.
package generate

import (
	"fmt"
	"strings"

	"github.com/faultbox/Faultbox/internal/star"
)

// Analysis holds the extracted topology and scenario information.
type Analysis struct {
	Services  []ServiceInfo
	Edges     []DependencyEdge
	Scenarios []ScenarioInfo
	// TODO: Covered — already tested fault combinations (for deduplication)
}

// ServiceInfo describes a service from the topology.
type ServiceInfo struct {
	Name       string
	VarName    string // Starlark variable name (may differ from service name)
	Protocol   string // primary interface protocol
	Interfaces []InterfaceInfo
}

// InterfaceInfo describes a service interface.
type InterfaceInfo struct {
	Name     string
	Protocol string
	Port     int
}

// DependencyEdge represents a dependency between two services.
type DependencyEdge struct {
	From     string // service that depends (makes outbound calls)
	To       string // service depended on (receives calls)
	Via      string // how discovered: "depends_on", "env"
	Protocol string // protocol of the target interface
}

// ScenarioInfo describes a registered scenario() function.
type ScenarioInfo struct {
	Name    string // function name (e.g., "order_flow")
	VarName string // Starlark variable name (same as Name for functions)
}

// Analyze extracts topology, dependencies, and scenarios from a loaded runtime.
func Analyze(rt *star.Runtime) (*Analysis, error) {
	a := &Analysis{}

	// Extract services and their interfaces.
	servicesByName := make(map[string]*ServiceInfo)
	for _, svc := range rt.Services() {
		si := ServiceInfo{
			Name:    svc.Name,
			VarName: svc.Name, // variable name typically matches service name
		}
		for _, iface := range svc.Interfaces {
			si.Interfaces = append(si.Interfaces, InterfaceInfo{
				Name:     iface.Name,
				Protocol: iface.Protocol,
				Port:     iface.Port,
			})
			// Use the first interface's protocol as the primary.
			if si.Protocol == "" {
				si.Protocol = iface.Protocol
			}
		}
		a.Services = append(a.Services, si)
		servicesByName[svc.Name] = &a.Services[len(a.Services)-1]
	}

	// Extract dependency edges.
	for _, svc := range rt.Services() {
		// Explicit depends_on.
		for _, dep := range svc.DependsOn {
			protocol := ""
			if depSvc, ok := servicesByName[dep]; ok && depSvc.Protocol != "" {
				protocol = depSvc.Protocol
			}
			a.Edges = append(a.Edges, DependencyEdge{
				From:     svc.Name,
				To:       dep,
				Via:      "depends_on",
				Protocol: protocol,
			})
		}

		// Environment variable wiring — look for references to other services.
		for _, val := range svc.Env {
			for _, other := range rt.Services() {
				if other.Name == svc.Name {
					continue
				}
				// Check if env value references another service's address.
				for _, iface := range other.Interfaces {
					addr := strings.ToLower(fmt.Sprintf("%s:%d", other.Name, iface.Port))
					lowerVal := strings.ToLower(val)
					if strings.Contains(lowerVal, addr) ||
						strings.Contains(lowerVal, strings.ToLower(other.Name)+".") {
						// Avoid duplicate edges.
						if !hasEdge(a.Edges, svc.Name, other.Name) {
							a.Edges = append(a.Edges, DependencyEdge{
								From:     svc.Name,
								To:       other.Name,
								Via:      "env",
								Protocol: iface.Protocol,
							})
						}
					}
				}
			}
		}
	}

	// Extract registered scenarios.
	for _, s := range rt.Scenarios() {
		a.Scenarios = append(a.Scenarios, ScenarioInfo{
			Name:    s.Name,
			VarName: s.Name,
		})
	}

	return a, nil
}

func hasEdge(edges []DependencyEdge, from, to string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}
