package star

import (
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// TestStepSummaryPrefersSQLPreview verifies the v0.12.2 step-event
// summary string used in the report's lane tooltip and detail row.
// Without a meaningful preview the user sees only `← db.exec` and
// can't tell what the step actually did — the regression Boris
// flagged on the test_order_feed lane.
func TestStepSummaryPrefersSQLPreview(t *testing.T) {
	args := map[string]any{"sql": "SELECT * FROM orders WHERE id = ?", "params": []int{42}}
	got := stepSummary("db", "exec", args, nil, true)
	if !strings.Contains(got, "→ db.exec") {
		t.Errorf("send arrow + target.method missing: %q", got)
	}
	if !strings.Contains(got, "SELECT") {
		t.Errorf("SQL preview missing: %q", got)
	}
}

// TestStepSummaryRecvCarriesStatus checks that the recv-direction
// summary includes the HTTP-style status code and the receive-arrow
// marker so the user can tell sent-vs-received at a glance.
func TestStepSummaryRecvCarriesStatus(t *testing.T) {
	args := map[string]any{"path": "/orders/42"}
	res := &protocol.StepResult{StatusCode: 200, Success: true, DurationMs: 17}
	got := stepSummary("api", "get", args, res, false)
	if !strings.Contains(got, "← api.get") {
		t.Errorf("recv arrow missing: %q", got)
	}
	if !strings.Contains(got, "/orders/42") {
		t.Errorf("path preview missing: %q", got)
	}
	if !strings.Contains(got, "200") {
		t.Errorf("status code missing: %q", got)
	}
}

// TestStepSummaryFlagsErrors surfaces the failure mode in the lane
// tooltip — the user shouldn't have to expand the drill-down to find
// out a step errored. Truncation keeps the line in the tooltip width.
func TestStepSummaryFlagsErrors(t *testing.T) {
	args := map[string]any{"path": "/orders"}
	res := &protocol.StepResult{Success: false, Error: "context deadline exceeded"}
	got := stepSummary("api", "get", args, res, false)
	if !strings.Contains(got, "ERR:") {
		t.Errorf("error indicator missing: %q", got)
	}
	if !strings.Contains(got, "deadline") {
		t.Errorf("error message missing: %q", got)
	}
}

// TestMergeStepKwargFieldsRespectsAllowlist guards against accidental
// inclusion of arbitrary kwargs in the bundle — keeps each event
// small and predictable.
func TestMergeStepKwargFieldsRespectsAllowlist(t *testing.T) {
	fields := map[string]string{"target": "db", "method": "exec"}
	args := map[string]any{
		"sql":      "INSERT INTO t VALUES (?)",
		"args":     "[1]",
		"_secret":  "leaked-token",
		"password": "hunter2",
	}
	mergeStepKwargFields(fields, args)
	if fields["sql"] == "" {
		t.Error("sql should propagate")
	}
	if fields["args"] == "" {
		t.Error("args should propagate")
	}
	if _, leaked := fields["_secret"]; leaked {
		t.Error("_secret must not propagate (not in allowlist)")
	}
	if _, leaked := fields["password"]; leaked {
		t.Error("password must not propagate (not in allowlist)")
	}
}

// TestTruncateLeavesShortStringsAlone — sanity check the truncation
// helper used across the enriched step fields.
func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	if got := truncate("hello", 200); got != "hello" {
		t.Errorf("short string mangled: %q", got)
	}
	long := strings.Repeat("a", 250)
	got := truncate(long, 200)
	if len(got) <= 200 {
		t.Errorf("truncated form should include the … marker, got length %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated form should end with ellipsis, got %q", got[len(got)-5:])
	}
}
