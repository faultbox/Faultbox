package config

import (
	"fmt"
	"regexp"
	"strings"
)

var templateRe = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// ResolveEnv builds the final environment variable list for a service.
// It merges user-defined env vars with auto-injected FAULTBOX_* vars for
// service discovery, and resolves {{service.interface.addr}} templates.
func ResolveEnv(topo *TopologyConfig, serviceName string) ([]string, error) {
	svc := topo.Services[serviceName]
	env := make(map[string]string)

	// 1. Auto-inject FAULTBOX_<SERVICE>_<INTERFACE>_* for all services.
	for name, s := range topo.Services {
		upper := strings.ToUpper(name)
		for ifName, iface := range s.Interfaces {
			ifUpper := strings.ToUpper(ifName)
			prefix := fmt.Sprintf("FAULTBOX_%s_%s", upper, ifUpper)
			env[prefix+"_HOST"] = "localhost"
			env[prefix+"_PORT"] = fmt.Sprintf("%d", iface.Port)
			env[prefix+"_ADDR"] = fmt.Sprintf("localhost:%d", iface.Port)
		}
	}

	// 2. Merge user-defined env vars (override auto-injected if same key).
	for k, v := range svc.Environment {
		resolved, err := resolveTemplates(topo, v)
		if err != nil {
			return nil, fmt.Errorf("service %q env %q: %w", serviceName, k, err)
		}
		env[k] = resolved
	}

	// 3. Convert to KEY=VALUE slice.
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result, nil
}

// resolveTemplates replaces {{service.interface.field}} with actual values.
// Supported: {{service.interface.addr}}, {{service.interface.host}}, {{service.interface.port}}
// Shorthand: {{service.addr}} when service has a single interface.
func resolveTemplates(topo *TopologyConfig, s string) (string, error) {
	var resolveErr error
	result := templateRe.ReplaceAllStringFunc(s, func(match string) string {
		if resolveErr != nil {
			return match
		}
		inner := strings.TrimSpace(match[2 : len(match)-2])
		parts := strings.Split(inner, ".")

		var svcName, ifName, field string
		switch len(parts) {
		case 2:
			// {{service.field}} — shorthand for single-interface services
			svcName, field = parts[0], parts[1]
		case 3:
			// {{service.interface.field}}
			svcName, ifName, field = parts[0], parts[1], parts[2]
		default:
			resolveErr = fmt.Errorf("invalid template %q: expected service.field or service.interface.field", match)
			return match
		}

		svc, ok := topo.Services[svcName]
		if !ok {
			resolveErr = fmt.Errorf("template %q: service %q not found", match, svcName)
			return match
		}

		// Resolve interface.
		var iface InterfaceConfig
		if ifName == "" {
			if len(svc.Interfaces) == 1 {
				for _, ic := range svc.Interfaces {
					iface = ic
				}
			} else if ic, ok := svc.Interfaces["default"]; ok {
				iface = ic
			} else {
				resolveErr = fmt.Errorf("template %q: service %q has multiple interfaces, specify which one", match, svcName)
				return match
			}
		} else {
			ic, ok := svc.Interfaces[ifName]
			if !ok {
				resolveErr = fmt.Errorf("template %q: service %q interface %q not found", match, svcName, ifName)
				return match
			}
			iface = ic
		}

		switch field {
		case "addr":
			return fmt.Sprintf("localhost:%d", iface.Port)
		case "host":
			return "localhost"
		case "port":
			return fmt.Sprintf("%d", iface.Port)
		default:
			resolveErr = fmt.Errorf("template %q: unknown field %q (use addr, host, or port)", match, field)
			return match
		}
	})

	return result, resolveErr
}
