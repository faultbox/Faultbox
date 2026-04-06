package generate

import "fmt"

// Mutation describes a single failure scenario — one scenario wrapped in one fault.
type Mutation struct {
	Name        string // test function name: test_gen_<scenario>_<target>_<fault>
	Scenario    string // scenario function name to wrap
	Category    string // "network", "disk"
	Description string // docstring
	FaultTarget string // service variable to fault
	Syscall     string // "connect", "write", "fsync", "read"
	Action      string // "deny", "delay"
	Errno       string // "ECONNREFUSED", "EIO", etc. (empty for delay)
	Delay       string // "5s" etc. (empty for deny)
	Label       string // human-readable fault label
	Partition   bool   // use partition() instead of fault()
	PartitionA  string // first service for partition
	PartitionB  string // second service for partition
	Severity    string // "critical", "high", "medium"
}

// BuildMatrix generates all failure mutations from the analysis.
func BuildMatrix(a *Analysis) []Mutation {
	var mutations []Mutation

	for _, scenario := range a.Scenarios {
		// For each dependency edge, generate network and disk failures.
		for _, edge := range a.Edges {
			mutations = append(mutations, networkMutations(scenario, edge)...)
			mutations = append(mutations, diskMutations(scenario, edge)...)
		}
	}

	return mutations
}

// networkMutations generates network failure scenarios for a dependency edge.
func networkMutations(scenario ScenarioInfo, edge DependencyEdge) []Mutation {
	from := edge.From
	to := edge.To

	return []Mutation{
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_down", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "network",
			Description: fmt.Sprintf("%s with %s connection refused.", scenario.Name, to),
			FaultTarget: from,
			Syscall:     "connect",
			Action:      "deny",
			Errno:       "ECONNREFUSED",
			Label:       to + " down",
			Severity:    "critical",
		},
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_slow", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "network",
			Description: fmt.Sprintf("%s with %s delayed 5s.", scenario.Name, to),
			FaultTarget: from,
			Syscall:     "connect",
			Action:      "delay",
			Delay:       "5s",
			Label:       to + " slow",
			Severity:    "high",
		},
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_reset", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "network",
			Description: fmt.Sprintf("%s with %s dropping mid-request.", scenario.Name, to),
			FaultTarget: from,
			Syscall:     "read",
			Action:      "deny",
			Errno:       "ECONNRESET",
			Label:       to + " connection reset",
			Severity:    "high",
		},
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_partition", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "network",
			Description: fmt.Sprintf("%s with network partition between %s and %s.", scenario.Name, from, to),
			Partition:   true,
			PartitionA:  from,
			PartitionB:  to,
			Severity:    "critical",
		},
	}
}

// diskMutations generates disk failure scenarios for the target service.
func diskMutations(scenario ScenarioInfo, edge DependencyEdge) []Mutation {
	to := edge.To

	return []Mutation{
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_io_error", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "disk",
			Description: fmt.Sprintf("%s with %s disk I/O error.", scenario.Name, to),
			FaultTarget: to,
			Syscall:     "write",
			Action:      "deny",
			Errno:       "EIO",
			Label:       "disk I/O error",
			Severity:    "critical",
		},
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_disk_full", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "disk",
			Description: fmt.Sprintf("%s with %s disk full.", scenario.Name, to),
			FaultTarget: to,
			Syscall:     "write",
			Action:      "deny",
			Errno:       "ENOSPC",
			Label:       "disk full",
			Severity:    "high",
		},
		{
			Name:        fmt.Sprintf("test_gen_%s_%s_fsync_fail", scenario.Name, to),
			Scenario:    scenario.Name,
			Category:    "disk",
			Description: fmt.Sprintf("%s with %s fsync failure.", scenario.Name, to),
			FaultTarget: to,
			Syscall:     "fsync",
			Action:      "deny",
			Errno:       "EIO",
			Label:       "fsync failure",
			Severity:    "critical",
		},
	}
}
