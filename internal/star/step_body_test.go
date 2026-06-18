package star

import (
	"strings"
	"testing"
)

// TestErrorBodyForTrace covers the F-3 (v0.13.0 eval) rule for putting
// a response body on step_recv events: record it on non-2xx with a
// non-empty body, skip it otherwise, and cap the recorded length.
func TestErrorBodyForTrace(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantOK   bool
		wantBody string // expected recorded body when wantOK
	}{
		{"400 with body", 400, `{"error":"bad request"}`, true, `{"error":"bad request"}`},
		{"500 with body", 500, "internal error", true, "internal error"},
		{"200 omitted", 200, "ok", false, ""},
		{"201 omitted", 201, "created", false, ""},
		{"204 no content omitted", 204, "", false, ""},
		{"non-2xx empty body omitted", 503, "", false, ""},
		{"zero status omitted", 0, "tcp payload", false, ""},
		{"3xx recorded", 304, "not modified body", true, "not modified body"},
	}
	for _, tc := range cases {
		got, ok := errorBodyForTrace(tc.status, tc.body)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.wantBody {
			t.Errorf("%s: body = %q, want %q", tc.name, got, tc.wantBody)
		}
	}

	// Oversized bodies are capped at 2KB with a truncation marker so a
	// large error page can't bloat the bundle.
	big := strings.Repeat("x", 5000)
	got, ok := errorBodyForTrace(502, big)
	if !ok {
		t.Fatal("oversized non-2xx body should be recorded")
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 2048)) {
		t.Errorf("oversized body should keep first 2048 bytes, got prefix len %d", len(got))
	}
	if len(got) >= len(big) {
		t.Errorf("oversized body should be truncated, got len %d (input %d)", len(got), len(big))
	}
}
