//go:build linux

package netfault

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// Orphan TUN devices, and why reaping them is not optional.
//
// A gateway removes its device on the normal teardown path, and v0.14.1 also
// removes it on SIGINT/SIGTERM. Neither covers SIGKILL, an OOM kill, or a
// panic in the wrong goroutine — and a TUN device created with `ip tuntap add`
// is persistent, so it outlives the process unconditionally.
//
// In v0.14.0 the device name was the constant "faultbox0", which turned that
// leak into a host-level outage: every later packet-fault run failed with
//
//	start packet gateway: TUNSETIFF faultbox0: device or resource busy
//
// naming a device the user had never heard of, with no documented recovery.
// Worse, the failure is silent in the way that matters — the run continues,
// installs its packet rules into a gateway that is not there, and every fault
// quietly affects nothing. (v0.14.0's unwired-gateway check is what stops that
// being reported as a pass.)
//
// Per-process names (fbox<pid>) make a leak harmless rather than fatal. This
// sweep is the second half: it removes the litter, so a machine recovers on
// its own instead of needing `ip link delete` from someone who knows.

// reapOrphanDevices removes every fbox<pid> TUN device whose owning process is
// gone. Devices belonging to live processes are left alone — concurrent runs
// are legitimate, and that is the whole point of per-process naming.
//
// Best-effort by design: a device that cannot be enumerated or removed is
// logged and skipped, never fatal. Failing to tidy up after a previous run is
// not a reason to refuse this one.
func reapOrphanDevices() {
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Debug("packet gateway: could not enumerate interfaces for orphan sweep",
			"error", err.Error())
		return
	}
	self := os.Getpid()
	for _, ifc := range ifaces {
		pid, ok := parseDeviceName(ifc.Name)
		if !ok || pid == self {
			continue
		}
		if processAlive(pid) {
			// A concurrent Faultbox run owns this one.
			continue
		}
		if err := run("ip", "link", "del", ifc.Name); err != nil {
			slog.Warn("packet gateway: could not remove orphaned TUN device",
				"device", ifc.Name, "owner_pid", pid, "error", err.Error())
			continue
		}
		slog.Info("packet gateway: removed orphaned TUN device left by an earlier run",
			"device", ifc.Name, "owner_pid", pid)
	}
}

// parseDeviceName extracts the owning pid from an "fbox<pid>" interface name.
//
// The prefix check alone is not enough: an unrelated interface could begin
// with "fbox", and deleting someone else's network device because its name
// looked familiar would be a far worse bug than the leak this fixes. Requiring
// the remainder to parse as a positive integer is what makes the name a claim
// of ownership rather than a coincidence.
func parseDeviceName(name string) (pid int, ok bool) {
	rest, found := strings.CutPrefix(name, DevicePrefix)
	if !found || rest == "" {
		return 0, false
	}
	// Digits only. strconv.Atoi accepts a leading sign, so "fbox+1" would
	// otherwise parse as pid 1 and we would claim ownership of an interface we
	// could not have created. Being strict here is the difference between
	// tidying our own litter and deleting a stranger's device.
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(rest)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid is a live process.
//
// /proc is the authority here rather than kill(pid, 0): the sweep may run as
// root, where signalling any pid succeeds regardless of who owns it, and a
// false "alive" would leave orphans forever.
func processAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}
