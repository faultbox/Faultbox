//go:build linux

package engine

import (
	"strconv"
	"strings"
)

// PID 1 ignores default-action TERM. Only skip the grace period when procfs
// proves both namespace-init status and the absence of a caught TERM signal.
func namespaceInitWithoutTermHandler(status string) bool {
	init, known, caught := false, false, uint64(0)
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "NSpid:":
			init = len(fields) > 2 && fields[len(fields)-1] == "1"
		case "SigCgt:":
			var err error
			caught, err = strconv.ParseUint(fields[1], 16, 64)
			known = err == nil
		}
	}
	return init && known && caught&(1<<14) == 0
}
