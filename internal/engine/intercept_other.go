//go:build !linux

package engine

import (
	"context"
	"fmt"
	"runtime"
)

func (s *Session) runWithFaults(_ context.Context) (*Result, error) {
	return nil, fmt.Errorf("syscall fault injection requires Linux (current OS: %s); use the Lima VM", runtime.GOOS)
}
