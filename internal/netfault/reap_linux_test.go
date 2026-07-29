//go:build linux

package netfault

import (
	"os"
	"os/exec"
	"testing"
)

// parseDeviceName is the guard between "tidy up our litter" and "delete a
// stranger's network device because the name looked familiar". Its negative
// cases matter more than its positive one.
func TestParseDeviceName(t *testing.T) {
	cases := []struct {
		name    string
		wantPID int
		wantOK  bool
	}{
		{"fbox1234", 1234, true},
		{"fbox1", 1, true},
		{"fbox4194304", 4194304, true}, // kernel default pid_max

		// Not ours, or not provably ours.
		{"fbox", 0, false},      // prefix with no pid
		{"fbox0", 0, false},     // pid 0 is not a real owner
		{"fbox-12", 0, false},   // not a bare integer
		{"fboxabc", 0, false},   // ditto
		{"fbox12a", 0, false},   // trailing junk
		{"fbox 12", 0, false},   // whitespace
		{"eth0", 0, false},      // unrelated
		{"lo", 0, false},        // unrelated
		{"faultbox0", 0, false}, // v0.14.0's name: no pid, so not reapable
		{"myfbox12", 0, false},  // prefix must be at the start
		{"fbox-1", 0, false},    // negative-looking
		{"fbox+1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, ok := parseDeviceName(tc.name)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && pid != tc.wantPID {
				t.Errorf("pid = %d, want %d", pid, tc.wantPID)
			}
		})
	}
}

// Round-trip: whatever deviceNameFor builds, parseDeviceName must claim.
// If these two ever disagree, every device Faultbox creates becomes
// unreapable and the leak is back with no symptom until a host fills up.
func TestDeviceNameRoundTrip(t *testing.T) {
	for _, pid := range []int{1, 42, 9999, 4194304, os.Getpid()} {
		name := deviceNameFor(pid)
		got, ok := parseDeviceName(name)
		if !ok {
			t.Errorf("deviceNameFor(%d) = %q, which parseDeviceName rejects", pid, name)
			continue
		}
		if got != pid {
			t.Errorf("round trip for %d gave %d (name %q)", pid, got, name)
		}
	}
}

// Linux caps interface names at IFNAMSIZ-1 = 15 bytes. A truncated name would
// silently belong to a different pid.
func TestDeviceNameFitsIFNAMSIZ(t *testing.T) {
	const maxIfName = 15
	for _, pid := range []int{1, 4194304, 999999999} {
		if n := len(deviceNameFor(pid)); n > maxIfName {
			t.Errorf("deviceNameFor(%d) is %d bytes, over the %d-byte limit", pid, n, maxIfName)
		}
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("the running test process must be reported alive")
	}

	// A reaped child is the closest thing to a guaranteed-dead pid.
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not spawn a child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Skipf("child did not exit cleanly: %v", err)
	}
	if processAlive(pid) {
		t.Errorf("pid %d exited and was reaped, but is reported alive", pid)
	}
}

// The default must be per-process. A shared constant is what made a leaked
// device fatal for every later run in v0.14.0.
func TestDefaultDeviceIsPerProcess(t *testing.T) {
	cfg := (&GatewayConfig{}).withDefaults()
	if want := deviceNameFor(os.Getpid()); cfg.Device != want {
		t.Errorf("default device = %q, want %q", cfg.Device, want)
	}
	if cfg.Device == "faultbox0" {
		t.Error("default device is still the v0.14.0 shared constant")
	}
	// An explicit name still wins.
	explicit := (&GatewayConfig{Device: "fbox999"}).withDefaults()
	if explicit.Device != "fbox999" {
		t.Errorf("explicit device overridden: got %q", explicit.Device)
	}
}

// reapOrphanDevices must never touch a device whose owner is alive — that is
// what makes concurrent runs on one host safe. Without CAP_NET_ADMIN it can
// still be called; it just finds nothing to remove and must not panic.
func TestReapOrphanDevices_LeavesLiveOwnersAlone(t *testing.T) {
	self := deviceNameFor(os.Getpid())
	if pid, ok := parseDeviceName(self); !ok || !processAlive(pid) {
		t.Fatalf("own device name %q must parse to this live process", self)
	}
	// Must be safe to call regardless of privileges or existing interfaces.
	reapOrphanDevices()
}
