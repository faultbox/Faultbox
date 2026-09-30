package connowner

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const platformSupported = true

var errProcessGone = errors.New("process exited")

type process struct {
	pid, parent int
	start       uint64
}

// The command name may contain spaces or ')' characters. Field 22 is the
// start time, so split only after the final closing parenthesis of comm.
func parseProcessStat(data string) (process, error) {
	left, right := strings.IndexByte(data, '('), strings.LastIndexByte(data, ')')
	if left < 1 || right <= left {
		return process{}, fmt.Errorf("invalid process stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(data[:left]))
	if err != nil || pid <= 0 {
		return process{}, fmt.Errorf("invalid process PID")
	}
	fields := strings.Fields(data[right+1:])
	if len(fields) < 20 {
		return process{}, fmt.Errorf("process stat is incomplete")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return process{}, errProcessGone
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return process{}, fmt.Errorf("invalid process parent")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return process{}, fmt.Errorf("invalid process start time")
	}
	return process{pid: pid, parent: parent, start: start}, nil
}

func readProcess(root string, pid int) (process, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return process{}, err
	}
	p, err := parseProcessStat(string(data))
	if err == nil && p.pid != pid {
		return process{}, fmt.Errorf("process PID changed")
	}
	return p, err
}

func processStart(root string, pid int) (uint64, error) {
	p, err := readProcess(root, pid)
	return p.start, err
}

func sameProcess(root string, p process) bool {
	current, err := readProcess(root, p.pid)
	return err == nil && current == p
}

// Walk only children listed by threads of a verified root. Never enumerate the
// global /proc PID directory; unrelated host processes are not candidates.
func children(root string, parent process) ([]process, error) {
	tasks, err := os.ReadDir(filepath.Join(root, strconv.Itoa(parent.pid), "task"))
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool)
	var result []process
	for _, task := range tasks {
		if _, err := strconv.Atoi(task.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(parent.pid), "task", task.Name(), "children"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			child, err := readProcess(root, pid)
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errProcessGone) {
				return nil, err
			}
			if err == nil && child.parent == parent.pid && child.start >= parent.start {
				result = append(result, child)
			}
		}
	}
	if !sameProcess(root, parent) {
		return nil, fmt.Errorf("process generation changed")
	}
	return result, nil
}

func ownsConnection(root string, reg registration, key tuple) (owned, complete bool) {
	p, err := readProcess(root, reg.source.PID)
	if err != nil {
		return false, errors.Is(err, os.ErrNotExist) || errors.Is(err, errProcessGone)
	}
	if p.start != reg.start {
		return false, true
	}
	seen := make(map[int]bool)
	var visit func(process) (bool, error)
	visit = func(current process) (bool, error) {
		if seen[current.pid] || !sameProcess(root, current) {
			return false, fmt.Errorf("process tree changed")
		}
		seen[current.pid] = true
		if len(seen) > 4096 {
			return false, fmt.Errorf("process tree too large to prove ownership")
		}
		owned, err := processOwnsTuple(root, current, key)
		if err != nil || owned {
			return owned, err
		}
		descendants, err := children(root, current)
		if err != nil {
			return false, err
		}
		for _, child := range descendants {
			owned, err := visit(child)
			if err != nil {
				return false, err
			}
			if owned && sameProcess(root, current) {
				return true, nil
			}
		}
		return false, nil
	}
	owned, err = visit(p)
	return owned && sameProcess(root, p), err == nil && sameProcess(root, p)
}

func processOwnsTuple(root string, p process, key tuple) (bool, error) {
	base := filepath.Join(root, strconv.Itoa(p.pid))
	entries, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		return false, err
	}
	inodes := make(map[uint64][]string)
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(base, "fd", entry.Name()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inode, err := strconv.ParseUint(target[len("socket:["):len(target)-1], 10, 64)
		if err == nil && inode != 0 {
			inodes[inode] = append(inodes[inode], entry.Name())
		}
	}
	if len(inodes) == 0 {
		return false, nil
	}
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(base, "net", name))
		if err != nil {
			if name == "tcp6" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			tuple, inode, ok := parseTCPRow(line, binary.NativeEndian)
			if ok && tuple == key && sameProcess(root, p) {
				for _, fd := range inodes[inode] {
					// The descriptor must still refer to this inode after the
					// network-table read; otherwise an inode reused after close
					// could be attributed to the former process owner.
					target, err := os.Readlink(filepath.Join(base, "fd", fd))
					if err == nil && target == fmt.Sprintf("socket:[%d]", inode) && sameProcess(root, p) {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

// Linux renders each address word in native byte order in both tcp tables.
// IPv6 is four independently ordered 32-bit words, not one reversed 128-bit
// integer. IPv4-mapped IPv6 normalizes to the same tuple as a tcp4 connection.
func parseProcEndpoint(value string, order binary.ByteOrder) (endpoint, bool) {
	host, portText, ok := strings.Cut(value, ":")
	if !ok || (len(host) != 8 && len(host) != 32) {
		return endpoint{}, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return endpoint{}, false
	}
	for i := 0; i < len(raw); i += 4 {
		word := order.Uint32(raw[i : i+4])
		binary.BigEndian.PutUint32(raw[i:i+4], word)
	}
	port, err := strconv.ParseUint(portText, 16, 16)
	if err != nil || port == 0 {
		return endpoint{}, false
	}
	ip, ok := netip.AddrFromSlice(raw)
	if !ok || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return endpoint{}, false
	}
	return endpoint{ip.Unmap(), uint16(port)}, true
}

func parseTCPRow(line string, order binary.ByteOrder) (tuple, uint64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 || fields[3] == "0A" || fields[3] == "06" {
		return tuple{}, 0, false
	}
	from, fromOK := parseProcEndpoint(fields[1], order)
	to, toOK := parseProcEndpoint(fields[2], order)
	inode, err := strconv.ParseUint(fields[9], 10, 64)
	return tuple{from, to}, inode, fromOK && toOK && err == nil && inode > 0
}
