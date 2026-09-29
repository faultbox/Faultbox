package star

import "fmt"

// Validate at execution time, after all modules/services are registered, but
// before starting anything. Existing setup= remains a post-start hook.
func (rt *Runtime) validateTestMockState(cfg *TestConfig) error {
	if cfg == nil {
		return nil
	}
	for name := range cfg.MockState {
		svc := rt.services[name]
		if svc == nil || !svc.IsMock() {
			return fmt.Errorf("mock_state: %q is not a registered mock service", name)
		}
		for _, consumer := range rt.services {
			if consumer.Reuse && rt.dependsOnMock(consumer.Name, name, make(map[string]bool)) {
				return fmt.Errorf("mock_state[%q] cannot configure reused dependent %q at boot; disable reuse for that consumer", name, consumer.Name)
			}
		}
	}
	return nil
}

func (rt *Runtime) dependsOnMock(service, target string, seen map[string]bool) bool {
	if service == target {
		return true
	}
	if seen[service] {
		return false
	}
	seen[service] = true
	svc := rt.services[service]
	if svc == nil {
		return false
	}
	for _, dep := range svc.DependsOn {
		if rt.dependsOnMock(dep, target, seen) {
			return true
		}
	}
	return false
}
