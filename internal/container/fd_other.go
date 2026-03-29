//go:build !linux

package container

import (
	"context"
	"fmt"
)

func waitForListenerFd(_ context.Context, _ string, _ int) (int, error) {
	return -1, fmt.Errorf("container seccomp fd retrieval requires Linux")
}
