package star

import (
	"fmt"
	"strings"
)

// SelectedTests validates an exact selection before any service is started.
// File order never changes execution order, seeds or plan-leaf enumeration.
func (rt *Runtime) SelectedTests(cfg RunConfig) ([]string, error) {
	all := rt.DiscoverTests()
	if cfg.Tests != nil {
		if cfg.Filter != "" {
			return nil, fmt.Errorf("--tests-from cannot be combined with --test")
		}
		if len(cfg.Tests) == 0 {
			return nil, fmt.Errorf("test list is empty")
		}
		known := make(map[string]bool, len(all))
		for _, name := range all {
			known[name] = true
		}
		selected := map[string]bool{}
		var unknown []string
		for _, name := range cfg.Tests {
			if !known[name] {
				unknown = append(unknown, name)
			}
			selected[name] = true
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("unknown exact test name(s): %s; use --dry-run to list names", strings.Join(unknown, ", "))
		}
		result := []string{}
		for _, name := range all {
			if selected[name] {
				result = append(result, name)
			}
		}
		return result, nil
	}
	matrixBases := map[string]bool{}
	for _, fs := range rt.faultScenarios {
		if fs.Matrix != nil {
			matrixBases["test_matrix_"+fs.Matrix.ScenarioName] = true
		}
	}
	result := []string{}
	for _, name := range all {
		if cfg.Filter == "" || matchTestFilter(name, cfg.Filter, matrixBases) {
			result = append(result, name)
		}
	}
	return result, nil
}
