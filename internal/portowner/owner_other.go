//go:build !linux

package portowner

import (
	"context"
	"fmt"
)

func lookup(context.Context, string, string, int) ([]Owner, error) {
	return nil, fmt.Errorf("listener ownership is unavailable on this platform")
}
func start(int) string { return "" }
