//go:build linux

package seccomp

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestBuildFilter_WriteFamilyWhitelist verifies the stderr-whitelist
// classification: write/writev/pwrite64 get the 6-instruction fd-check
// block when allowFdForWrite >= 0, regardless of what else is in the
// filter. Fixed in v0.8.7.
func TestBuildFilter_WriteFamilyWhitelist(t *testing.T) {
	writeNr := uint32(SyscallNumber("write"))
	prog := buildFilter([]uint32{writeNr}, 2, -1)

	// Find the JEQ nr instruction matching write.
	found := findJEQInstr(t, prog, writeNr)
	// Block shape: JEQ nr Jt=0 Jf=5 → 5 instructions ahead for the no-match
	// path. That's the signature of the whitelist block (vs 1-instruction
	// skip for unconditional NOTIFY).
	if prog[found].Jf != 5 {
		t.Errorf("write syscall should be in whitelist block (Jf=5), got Jf=%d", prog[found].Jf)
	}
}

// TestBuildFilter_SocketFamilyWhitelist verifies RFC-022 v0.9.1: when
// allowFdForSocket is set, read/readv/pread64/sendmsg/sendto/recvmsg/
// recvfrom each get the fd-check block so the shim's SCM_RIGHTS send
// and ACK read don't deadlock against the user's fault filter.
func TestBuildFilter_SocketFamilyWhitelist(t *testing.T) {
	for _, name := range []string{"read", "readv", "pread64", "sendmsg", "sendto", "recvmsg", "recvfrom"} {
		t.Run(name, func(t *testing.T) {
			nr := uint32(SyscallNumber(name))
			if int32(nr) < 0 {
				t.Skipf("syscall %q not on this arch", name)
			}
			prog := buildFilter([]uint32{nr}, -1, 5)
			idx := findJEQInstr(t, prog, nr)
			if prog[idx].Jf != 5 {
				t.Errorf("%s should be in socket whitelist block (Jf=5), got Jf=%d", name, prog[idx].Jf)
			}
		})
	}
}

// TestBuildFilter_SocketFamilyNotWhitelistedWhenFdIsNegative verifies
// that passing -1 for allowFdForSocket disables the socket exception
// entirely — read/sendmsg/etc. use the simple 2-instruction NOTIFY
// path. Important so the binary-mode (non-shim) path isn't affected
// by the v0.9.1 changes.
func TestBuildFilter_SocketFamilyNotWhitelistedWhenFdIsNegative(t *testing.T) {
	readNr := uint32(SyscallNumber("read"))
	prog := buildFilter([]uint32{readNr}, -1, -1)
	idx := findJEQInstr(t, prog, readNr)
	// Simple block: Jf=1 (skip the single NOTIFY instruction).
	if prog[idx].Jf != 1 {
		t.Errorf("read with allowFdForSocket=-1 should use simple block (Jf=1), got Jf=%d", prog[idx].Jf)
	}
}

// TestBuildFilter_UnrelatedSyscallNotWhitelisted verifies classification
// is narrow: a syscall that isn't in the write- or socket-family gets
// the simple NOTIFY path regardless of what whitelists are set. Covers
// the exact-match invariant — we never accidentally whitelist fsync/
// connect/etc. on the socket fd.
func TestBuildFilter_UnrelatedSyscallNotWhitelisted(t *testing.T) {
	fsyncNr := uint32(SyscallNumber("fsync"))
	prog := buildFilter([]uint32{fsyncNr}, 2, 5)
	idx := findJEQInstr(t, prog, fsyncNr)
	if prog[idx].Jf != 1 {
		t.Errorf("fsync should not be whitelisted (Jf=1 expected), got Jf=%d", prog[idx].Jf)
	}
}

// findJEQInstr returns the index of the first JEQ instruction with K==nr.
// Used by the whitelist-classification tests above to inspect block shape.
func findJEQInstr(t *testing.T, prog []unix.SockFilter, nr uint32) int {
	t.Helper()
	for i, ins := range prog {
		if ins.Code == (bpfJMP|bpfJEQ|bpfK) && ins.K == nr {
			return i
		}
	}
	t.Fatalf("JEQ nr=%d not found in program", nr)
	return -1
}
