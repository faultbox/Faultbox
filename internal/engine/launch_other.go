//go:build !linux

package engine

import (
	"context"
	"fmt"
	"runtime"
)

func (s *Session) launch(_ context.Context) (*Result, error) {
	return nil, fmt.Errorf("faultbox requires Linux (current OS: %s); use the Lima VM: make env-exec CMD=\"...\"", runtime.GOOS)
}
