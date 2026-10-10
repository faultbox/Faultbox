package portowner

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func procStart(data string) string {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return ""
	}
	f := strings.Fields(data[end+1:])
	if len(f) < 20 {
		return ""
	}
	return f[19]
}
func start(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	return procStart(string(data))
}
func localAddress(value string) (netip.Addr, int, bool) {
	a, p, ok := strings.Cut(value, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(a)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		word := binary.NativeEndian.Uint32(raw[i : i+4])
		binary.BigEndian.PutUint32(raw[i:i+4], word)
	}
	addr, ok := netip.AddrFromSlice(raw)
	port, err := strconv.ParseInt(p, 16, 32)
	return addr.Unmap(), int(port), ok && err == nil
}
func lookup(ctx context.Context, network, host string, port int) ([]Owner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	tables := []string{"tcp", "tcp6"}
	if strings.HasPrefix(network, "udp") {
		tables = []string{"udp", "udp6"}
	}
	inodes := map[string]bool{}
	for _, table := range tables {
		data, err := os.ReadFile("/proc/net/" + table)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 {
				continue
			}
			addr, p, ok := localAddress(f[1])
			if !ok || p != port {
				continue
			}
			if !target.IsUnspecified() && !addr.IsUnspecified() && addr != target {
				continue
			}
			if strings.HasPrefix(network, "tcp") && f[3] != "0A" {
				continue
			}
			if f[9] != "0" {
				inodes["socket:["+f[9]+"]"] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil, fmt.Errorf("no visible listener inode (owner may have exited or be outside visible procfs)")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := []Owner{}
	incomplete := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stamp := start(pid)
		if stamp == "" {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		dirs := []string{filepath.Join(base, "fd")}
		// /proc/PID/fd can disappear when the leader exits but workers live.
		tasks, _ := os.ReadDir(filepath.Join(base, "task"))
		for _, t := range tasks {
			dirs = append(dirs, filepath.Join(base, "task", t.Name(), "fd"))
		}
		found := false
		for _, dir := range dirs {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			fds, err := os.ReadDir(dir)
			if err != nil {
				if os.IsPermission(err) {
					incomplete = true
				}
				continue
			}
			for _, fd := range fds {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				path := filepath.Join(dir, fd.Name())
				target, err := os.Readlink(path)
				if os.IsPermission(err) {
					incomplete = true
				}
				if err != nil || !inodes[target] {
					continue
				}
				again, _ := os.Readlink(path)
				if again == target && start(pid) == stamp {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			comm, _ := os.ReadFile(filepath.Join(base, "comm"))
			result = append(result, Owner{PID: pid, Command: strings.TrimSpace(string(comm)), Start: stamp})
		}
	}
	if incomplete {
		return result, fmt.Errorf("ownership scan incomplete: some process descriptors are not readable")
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("listener owner unresolved (process changed or descriptors are not visible)")
	}
	return result, nil
}
