//go:build !linux

package container

import (
	"context"
	"fmt"
	"log/slog"
)

func waitForListenerFd(_ context.Context, _ string, _ *slog.Logger) (int, error) {
	return -1, fmt.Errorf("container seccomp fd retrieval requires Linux")
}
