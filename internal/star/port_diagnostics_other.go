//go:build !linux && !darwin

package star

import (
	"fmt"
	"runtime"
)

func hostEphemeralPortRange() (int, int, error) {
	return 0, 0, fmt.Errorf("host ephemeral port range unavailable on %s", runtime.GOOS)
}
