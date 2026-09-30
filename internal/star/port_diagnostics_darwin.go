package star

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func hostEphemeralPortRange() (int, int, error) {
	first, err := unix.SysctlUint32("net.inet.ip.portrange.first")
	if err != nil {
		return 0, 0, err
	}
	last, err := unix.SysctlUint32("net.inet.ip.portrange.last")
	if err != nil {
		return 0, 0, err
	}
	return parseEphemeralPortRange(fmt.Sprintf("%d %d", first, last))
}
