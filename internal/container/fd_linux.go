//go:build linux

package container

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// waitForListenerFd polls the report file for the listener fd number written
// by the container shim, then uses pidfd_getfd to copy it to this process.
func waitForListenerFd(ctx context.Context, reportPath string, hostPID int) (int, error) {
	var fdStr string
	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		default:
		}
		data, err := os.ReadFile(reportPath)
		if err == nil && len(data) > 0 {
			fdStr = string(data)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var childFd int
	_, err := fmt.Sscanf(fdStr, "%d", &childFd)
	if err != nil {
		return -1, fmt.Errorf("parse listener fd from %q: %w", fdStr, err)
	}
	if childFd <= 0 {
		return -1, fmt.Errorf("no seccomp listener fd reported (got %d)", childFd)
	}

	pidfd, err := unix.PidfdOpen(hostPID, 0)
	if err != nil {
		return -1, fmt.Errorf("pidfd_open(%d): %w", hostPID, err)
	}
	defer unix.Close(pidfd)

	localFd, err := unix.PidfdGetfd(pidfd, childFd, 0)
	if err != nil {
		return -1, fmt.Errorf("pidfd_getfd(pid=%d, fd=%d): %w", hostPID, childFd, err)
	}

	return localFd, nil
}
