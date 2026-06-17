//go:build linux

package seccomp

import (
	"strings"
	"testing"
)

// TestParseShimSignal covers the child→parent pipe protocol, including
// the "ERR <message>" shape added for F-2 (v0.13.0 eval): a missing
// target binary must fail the launch immediately with the root cause
// instead of surfacing as a healthcheck timeout a minute later.
func TestParseShimSignal(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantFd  int
		wantErr string // substring; empty = no error
	}{
		{"no filter", "0\n", 0, ""},
		{"listener fd", "5\n", 5, ""},
		{"exec enoent", "ERR exec /tmp/truck-api-bin: no such file or directory\n", -1,
			"launch target: exec /tmp/truck-api-bin: no such file or directory"},
		{"garbage", "bogus\n", -1, `parse listener fd "bogus"`},
		{"empty", "\n", -1, "parse listener fd"},
	}
	for _, tc := range cases {
		fd, err := parseShimSignal(tc.raw)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			if fd != tc.wantFd {
				t.Errorf("%s: fd = %d, want %d", tc.name, fd, tc.wantFd)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want substring %q", tc.name, err, tc.wantErr)
		}
	}
}
