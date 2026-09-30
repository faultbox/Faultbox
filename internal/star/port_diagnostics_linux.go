package star

import "os"

func hostEphemeralPortRange() (int, int, error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, 0, err
	}
	return parseEphemeralPortRange(string(data))
}
