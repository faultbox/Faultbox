// Package check validates a Faultbox spec without executing it (RFC-052 Gap 1).
//
// The runtime has always been able to do this: LoadFile parses the spec,
// resolves the topology, validates keyword arguments, and enforces the
// monitor/assume sandboxes — all without launching a process, pulling an image,
// or touching Docker. It simply was not exposed, so the only way to find out a
// spec was malformed was to run it and wait for services to start.
//
// For an agent that difference is the loop: write, check, fix, check. Every
// iteration needing a container is measured in tens of seconds instead of
// milliseconds.
//
// It lives in its own package so the CLI and the MCP tool share exactly this
// path. A check that behaved differently through MCP than through the CLI would
// be worse than no check, because the agent's model of the tool would be wrong.
//
// # What it will not do
//
// It will not tell you whether your suite is any good. NO_POSITIVE_CONTROL and
// TEST_NO_ASSERTIONS need a run — deciding whether an assertion is positive or
// negative from the source alone is unreliable, and a check that guessed would
// be worse than one that admits the boundary. See RFC-052 open question 7.
package check

import (
	"fmt"
	"log/slog"

	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/plan"
	"github.com/faultbox/Faultbox/internal/star"
)

// Finding is one result. Shares Level/Code/Message/Suggestion with
// star.Diagnostic so an agent parses one shape for check output and run output
// alike — which was the point of giving load-time errors codes at all.
type Finding struct {
	Level      string `json:"level"` // "error" | "warning" | "info"
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Result is the whole output of a check run.
type Result struct {
	SchemaVersion int       `json:"schema_version"`
	Spec          string    `json:"spec"`
	OK            bool      `json:"ok"`
	Tests         []string  `json:"tests"`
	PlanInstances int       `json:"plan_instances"`
	Findings      []Finding `json:"findings,omitempty"`
}

// Run loads and analyses a spec without launching anything.
//
// Exported so the MCP tool and the tests share exactly this path — a check that
// behaved differently through MCP than through the CLI would be worse than no
// check, because the agent's model of the tool would be wrong.
func Run(specFile string, maxInstances int) *Result {
	res := &Result{SchemaVersion: 1, Spec: specFile}

	logger := logging.New(logging.Config{Level: slog.LevelError})
	rt := star.New(logger)

	if err := rt.LoadFile(specFile); err != nil {
		res.Findings = append(res.Findings, findingFor(err))
		return res
	}

	res.Tests = rt.DiscoverTests()

	// Plan enumeration is static — it walks declared tests and their fan-out
	// without executing a body. Included by default (RFC-052 open question 4)
	// because a choose() cross-product that multiplies to thousands of leaves
	// is exactly the mistake an agent makes and cannot see, and finding it
	// costs nothing here.
	pt := plan.Enumerate(rt)
	if pt != nil {
		res.PlanInstances = pt.Totals.Instances
		if maxInstances > 0 && pt.Totals.Instances > maxInstances {
			res.Findings = append(res.Findings, Finding{
				Level: "error",
				Code:  "PLAN_COST_EXCEEDED",
				Message: fmt.Sprintf("plan expands to %d instances, over the limit of %d",
					pt.Totals.Instances, maxInstances),
				Suggestion: "Reduce a choose() axis, or raise --max-instances if the cost is " +
					"intended. `faultbox plan --format=text` shows which axes multiply.",
			})
		}
	}

	if len(res.Tests) == 0 {
		res.Findings = append(res.Findings, Finding{
			Level:   "warning",
			Code:    "NO_TESTS_DISCOVERED",
			Message: "the spec declares no tests",
			Suggestion: "Faultbox discovers functions named test_*, and tests declared with " +
				"test(\"name\", body=...). A spec with none loads successfully and then does nothing.",
		})
	}

	res.OK = !hasError(res.Findings)
	return res
}

func hasError(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == "error" {
			return true
		}
	}
	return false
}

// findingFor renders a load error, using its code when it carries one.
func findingFor(err error) Finding {
	if d := star.DiagnosticFor(err); d != nil {
		return Finding{
			Level:      d.Level,
			Code:       d.Code,
			Message:    d.Message,
			Suggestion: d.Suggestion,
		}
	}
	// Uncoded: report it plainly rather than inventing a classification. The
	// taxonomy is adopted incrementally and this is what a gap looks like.
	return Finding{
		Level:   "error",
		Code:    "SPEC_LOAD_FAILED",
		Message: err.Error(),
	}
}
