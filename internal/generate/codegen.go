package generate

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// assumptionDef represents a unique fault assumption extracted from mutations.
type assumptionDef struct {
	varName  string
	target   string
	syscall  string
	action   string
	errno    string
	delay    string
	label    string
	category string
}

// GenerateOpts controls code generation.
type GenerateOpts struct {
	Scenario string // filter to one scenario (empty = all)
	Service  string // filter to one dependency (empty = all)
	Category string // "network", "disk", "all" (empty = all)
	Source   string // source .star filename for load() statement
}

// Generate renders mutations as Starlark code using fault_assumption() + fault_matrix().
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
	sb.WriteString("# Uses fault_assumption() + fault_matrix() for composable testing.\n")
	sb.WriteString("# Add overrides= to fault_matrix() for per-cell expectations.\n")
	sb.WriteString("# ============================================================\n\n")

	// load() statement — import services and scenario functions.
	if opts.Source != "" {
		symbols := collectSymbols(filtered, analysis)
		fmt.Fprintf(&sb, "load(%q, %s)\n\n", opts.Source, joinQuoted(symbols))
	}

	// Extract unique fault assumptions from mutations.
	// Key: label (unique per fault mode, e.g., "db_down", "db_slow").
	seen := make(map[string]bool)
	var assumptions []assumptionDef
	var partitions []Mutation // partitions handled separately
	scenarioSet := make(map[string]bool)
	var scenarioOrder []string

	for _, m := range filtered {
		if !scenarioSet[m.Scenario] {
			scenarioSet[m.Scenario] = true
			scenarioOrder = append(scenarioOrder, m.Scenario)
		}

		if m.Partition {
			if !seen[m.Name] {
				seen[m.Name] = true
				partitions = append(partitions, m)
			}
			continue
		}

		// Build a unique var name from the label.
		varName := sanitizeVarName(m.Label)
		if seen[varName] {
			continue
		}
		seen[varName] = true
		assumptions = append(assumptions, assumptionDef{
			varName:  varName,
			target:   m.FaultTarget,
			syscall:  m.Syscall,
			action:   m.Action,
			errno:    m.Errno,
			delay:    m.Delay,
			label:    m.Label,
			category: m.Category,
		})
	}

	// Render fault_assumption() definitions grouped by category.
	byCategory := make(map[string][]assumptionDef)
	for _, a := range assumptions {
		cat := a.category
		if cat == "" {
			cat = "other"
		}
		byCategory[cat] = append(byCategory[cat], a)
	}

	sb.WriteString("# --- Fault Assumptions ---\n\n")
	for _, cat := range sortedKeys2(byCategory) {
		defs := byCategory[cat]
		fmt.Fprintf(&sb, "# %s faults\n", cat)
		for _, a := range defs {
			if a.action == "deny" {
				fmt.Fprintf(&sb, "%s = fault_assumption(%q,\n    target = %s,\n    %s = deny(%q),\n)\n\n",
					a.varName, a.varName, a.target, a.syscall, a.errno)
			} else if a.action == "delay" {
				fmt.Fprintf(&sb, "%s = fault_assumption(%q,\n    target = %s,\n    %s = delay(%q),\n)\n\n",
					a.varName, a.varName, a.target, a.syscall, a.delay)
			}
		}
	}

	// Render fault_matrix() per scenario.
	if len(assumptions) > 0 {
		sb.WriteString("# --- Fault Matrix ---\n\n")

		// Collect assumption var names.
		var faultVars []string
		for _, a := range assumptions {
			faultVars = append(faultVars, a.varName)
		}

		fmt.Fprintf(&sb, "fault_matrix(\n")
		fmt.Fprintf(&sb, "    scenarios = [%s],\n", strings.Join(scenarioOrder, ", "))
		fmt.Fprintf(&sb, "    faults = [%s],\n", strings.Join(faultVars, ", "))
		fmt.Fprintf(&sb, ")\n\n")
	}

	// Render partition tests as standalone functions (not matrix-composable).
	if len(partitions) > 0 {
		sb.WriteString("# --- Network Partitions ---\n\n")
		for _, m := range partitions {
			renderPartition(&sb, m)
		}
	}

	return sb.String()
}

// sanitizeVarName converts a human label like "db down" to a valid Python variable "db_down".
func sanitizeVarName(label string) string {
	s := strings.ToLower(label)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	// Remove non-alphanumeric/underscore chars.
	var result strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			result.WriteRune(c)
		}
	}
	return result.String()
}

func sortedKeys2(m map[string][]assumptionDef) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

func renderPartition(sb *strings.Builder, m Mutation) {
	// Partitions are not matrix-composable — rendered as standalone test functions.
	name := strings.Replace(m.Name, "test_gen_", "test_", 1)
	fmt.Fprintf(sb, "def %s():\n", name)
	fmt.Fprintf(sb, "    \"\"\"%s\"\"\"\n", m.Description)
	fmt.Fprintf(sb, "    partition(%s, %s, run=%s)\n",
		m.PartitionA, m.PartitionB, m.Scenario)
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
