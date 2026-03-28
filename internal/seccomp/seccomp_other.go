//go:build !linux

// Package seccomp provides seccomp-notify based syscall interception.
// This stub allows the package to be imported on non-Linux platforms.
package seccomp

import (
	"fmt"
	"os"
	"runtime"
)

// IsShimChild returns false on non-Linux — shim is never active.
func IsShimChild() bool { return false }

// RunShimChild is not supported on non-Linux.
func RunShimChild() error {
	return fmt.Errorf("seccomp shim requires Linux (current OS: %s)", runtime.GOOS)
}

// SyscallNumber returns -1 on non-Linux (no syscall table).
func SyscallNumber(name string) int32 { return -1 }

// SyscallName returns the number as a string on non-Linux.
func SyscallName(nr int32) string { return fmt.Sprintf("syscall_%d", nr) }

// Launch is not supported on non-Linux.
func Launch(_ LaunchConfig) (int, int, error) {
	return 0, -1, fmt.Errorf("launch requires Linux (current OS: %s)", runtime.GOOS)
}

// StartWithFilter is not supported on non-Linux.
func StartWithFilter(_ string, _ []string, _ []uint32, _ []*os.File) (int, int, error) {
	return 0, -1, fmt.Errorf("seccomp requires Linux (current OS: %s)", runtime.GOOS)
}
