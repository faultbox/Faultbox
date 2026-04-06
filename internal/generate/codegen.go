package generate

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// GenerateOpts controls code generation.
type GenerateOpts struct {
	Scenario string // filter to one scenario (empty = all)
	Service  string // filter to one dependency (empty = all)
	Category string // "network", "disk", "all" (empty = all)
	Source   string // source .star filename for load() statement
}

// Generate renders mutations as Starlark code.
func Generate(mutations []Mutation, analysis *Analysis, opts GenerateOpts) string {
	// Filter mutations.
	var filtered []Mutation
	for _, m := range mutations {
		if opts.Scenario != "" && m.Scenario != opts.Scenario {
			continue
		}
		if opts.Service != "" && m.FaultTarget != opts.Service &&
			m.PartitionA != opts.Service && m.PartitionB != opts.Service {
			continue
		}
		if opts.Category != "" && opts.Category != "all" && m.Category != opts.Category {
			continue
		}
		filtered = append(filtered, m)
	}

	if len(filtered) == 0 {
		return "# No mutations generated.\n"
	}

	var sb strings.Builder

	// Header.
	sb.WriteString("# ============================================================\n")
	sb.WriteString("# Auto-generated failure scenarios by: faultbox generate\n")
	if opts.Source != "" {
		fmt.Fprintf(&sb, "# Source: %s\n", opts.Source)
	}
	fmt.Fprintf(&sb, "# Generated: %s\n", time.Now().Format("2006-01-02"))
	sb.WriteString("#\n")
	sb.WriteString("# Review each test. The happy path's own assertions will\n")
	sb.WriteString("# either pass (system handles the fault) or fail (found a bug).\n")
	sb.WriteString("# Add assertions for the behavior you expect under failure.\n")
	sb.WriteString("# ============================================================\n\n")

	// load() statement — import services and scenario functions.
	if opts.Source != "" {
		symbols := collectSymbols(filtered, analysis)
		fmt.Fprintf(&sb, "load(%q, %s)\n\n", opts.Source, joinQuoted(symbols))
	}

	// Group mutations by category for readability.
	byCategory := groupByCategory(filtered)
	for _, cat := range sortedKeys(byCategory) {
		muts := byCategory[cat]
		fmt.Fprintf(&sb, "# --- %s failures ---\n\n", cat)
		for _, m := range muts {
			renderMutation(&sb, m)
		}
	}

	return sb.String()
}

// GeneratePerScenario returns a map of scenario name → generated Starlark code.
func GeneratePerScenario(mutations []Mutation, analysis *Analysis, opts GenerateOpts) map[string]string {
	// Group mutations by scenario.
	byScenario := make(map[string][]Mutation)
	for _, m := range mutations {
		byScenario[m.Scenario] = append(byScenario[m.Scenario], m)
	}

	result := make(map[string]string)
	for scenario, muts := range byScenario {
		sopts := opts
		sopts.Scenario = scenario
		result[scenario] = Generate(muts, analysis, sopts)
	}
	return result
}

// DryRun returns a human-readable summary of what would be generated.
func DryRun(mutations []Mutation, analysis *Analysis) string {
	// Group by scenario × dependency.
	type key struct{ scenario, target string }
	counts := make(map[key]int)
	for _, m := range mutations {
		target := m.FaultTarget
		if m.Partition {
			target = m.PartitionA + "↔" + m.PartitionB
		}
		counts[key{m.Scenario, target}]++
	}

	var sb strings.Builder
	for k, n := range counts {
		fmt.Fprintf(&sb, "  %s × %s: %d mutations\n", k.scenario, k.target, n)
	}
	fmt.Fprintf(&sb, "  Total: %d mutations\n", len(mutations))
	return sb.String()
}

func renderMutation(sb *strings.Builder, m Mutation) {
	// Docstring.
	fmt.Fprintf(sb, "def %s():\n", m.Name)
	fmt.Fprintf(sb, "    \"\"\"%s\"\"\"\n", m.Description)

	if m.Partition {
		fmt.Fprintf(sb, "    partition(%s, %s, run=%s)\n",
			m.PartitionA, m.PartitionB, m.Scenario)
	} else if m.Action == "deny" {
		fmt.Fprintf(sb, "    fault(%s, %s=deny(%q, label=%q), run=%s)\n",
			m.FaultTarget, m.Syscall, m.Errno, m.Label, m.Scenario)
	} else if m.Action == "delay" {
		fmt.Fprintf(sb, "    fault(%s, %s=delay(%q, label=%q), run=%s)\n",
			m.FaultTarget, m.Syscall, m.Delay, m.Label, m.Scenario)
	}
	sb.WriteString("\n")
}

// collectSymbols extracts the service and scenario names needed for load().
func collectSymbols(mutations []Mutation, analysis *Analysis) []string {
	seen := make(map[string]bool)
	for _, m := range mutations {
		seen[m.Scenario] = true
		if m.FaultTarget != "" {
			seen[m.FaultTarget] = true
		}
		if m.PartitionA != "" {
			seen[m.PartitionA] = true
		}
		if m.PartitionB != "" {
			seen[m.PartitionB] = true
		}
	}
	var symbols []string
	for s := range seen {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	return symbols
}

func joinQuoted(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}

func groupByCategory(mutations []Mutation) map[string][]Mutation {
	m := make(map[string][]Mutation)
	for _, mut := range mutations {
		cat := mut.Category
		if cat == "" {
			cat = "other"
		}
		m[cat] = append(m[cat], mut)
	}
	return m
}

func sortedKeys(m map[string][]Mutation) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
