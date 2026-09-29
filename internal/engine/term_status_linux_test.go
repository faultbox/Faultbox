//go:build linux

package engine

import "testing"

func TestNamespaceInitTerminationDecision(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"NSpid:\t100\t1\nSigCgt:\t0000000000000000\n", true},
		{"NSpid:\t100\t1\nSigCgt:\t0000000000004000\n", false},
		{"NSpid:\t100\t2\nSigCgt:\t0000000000000000\n", false},
		{"NSpid:\t1\nSigCgt:\t0000000000000000\n", false},
		{"NSpid:\t100\t1\nSigCgt:\tinvalid\n", false},
		{"", false},
	} {
		if got := namespaceInitWithoutTermHandler(tc.s); got != tc.want {
			t.Errorf("%q = %v", tc.s, got)
		}
	}
}
