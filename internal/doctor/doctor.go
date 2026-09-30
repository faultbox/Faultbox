// Package doctor provides bounded, read-only environment diagnostics. Passing
// checks establishes prerequisites, not proof that every fault mode will work.
package doctor

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type Check struct {
	Code    string `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}
type Report struct {
	Version    string  `json:"version"`
	Platform   string  `json:"platform"`
	Executable string  `json:"executable"`
	Checks     []Check `json:"checks"`
}

func (r Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == "error" {
			return false
		}
	}
	return true
}

type Options struct {
	Version, Executable, Lima string
	Docker, Packet, Trace     bool
}
type probe struct {
	os, arch string
	read     func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	command  func(context.Context, string, ...string) ([]byte, error)
}

func Run(ctx context.Context, o Options) Report {
	return run(ctx, o, probe{runtime.GOOS, runtime.GOARCH, os.ReadFile, os.Stat, command})
}
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeout := 8 * time.Second
	if name == "limactl" {
		timeout = 28 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Bound descendants holding inherited output pipes after cancellation.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}
func run(ctx context.Context, o Options, p probe) Report {
	r := Report{Version: o.Version, Platform: p.os + "/" + p.arch, Executable: o.Executable, Checks: []Check{}}
	add := func(code, status, msg, remedy string) { r.Checks = append(r.Checks, Check{code, status, msg, remedy}) }
	add("BINARY", "ok", fmt.Sprintf("Faultbox %s at %s", o.Version, o.Executable), "")
	if o.Lima != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(o.Lima) {
			add("LIMA_NAME", "error", "invalid Lima instance name", "Use the existing instance name, for example --lima=faultbox.")
			return r
		}
		args := []string{"shell", "--workdir", "/tmp", o.Lima, "faultbox", "doctor", "--format=json"}
		if o.Docker {
			args = append(args, "--docker")
		}
		if o.Packet {
			args = append(args, "--packet")
		}
		if o.Trace {
			args = append(args, "--trace")
		}
		out, err := p.command(ctx, "limactl", args...)
		var guest Report
		if decodeErr := json.Unmarshal(out, &guest); decodeErr != nil || guest.Version == "" || guest.Platform == "" || len(guest.Checks) == 0 {
			add("LIMA_DOCTOR", "error", fmt.Sprintf("cannot read doctor report from Lima %s (%v)", o.Lima, err), "Start the existing VM and install the same Faultbox release inside it. The guest must support `faultbox doctor`. See docs/guides/macos.md.")
			return r
		}
		if err != nil && guest.OK() {
			add("LIMA_COMMAND", "error", "guest command failed despite a passing report", "Run `limactl shell --workdir /tmp "+o.Lima+" faultbox doctor` to inspect the failure.")
		}
		if !strings.HasPrefix(guest.Platform, "linux/") {
			add("LIMA_PLATFORM", "error", "runner is not Linux: "+guest.Platform, "Select a Linux runner.")
		}
		if guest.Version != o.Version || guest.Version == "dev" {
			add("LIMA_VERSION", "error", fmt.Sprintf("host %s, guest %s", o.Version, guest.Version), "Install the same pinned release on host and runner; source builds need an explicit version label.")
		} else {
			add("LIMA_VERSION", "ok", "host and guest both "+o.Version, "")
		}
		for _, c := range guest.Checks {
			c.Code = "LIMA_" + c.Code
			r.Checks = append(r.Checks, c)
		}
		return r
	}
	if p.os != "linux" {
		add("LINUX_RUNNER", "error", "Native service execution requires Linux; this is "+r.Platform, "On macOS follow docs/guides/macos.md, then run `faultbox doctor --lima=faultbox --docker`.")
		return r
	}
	actions, err := p.read("/proc/sys/kernel/seccomp/actions_avail")
	if err != nil || !strings.Contains(" "+strings.TrimSpace(string(actions))+" ", " user_notif ") {
		add("SECCOMP_NOTIFY", "error", "kernel does not expose seccomp user notification", "Use a Linux kernel with seccomp-notify support (5.6+); check procfs visibility.")
	} else {
		add("SECCOMP_NOTIFY", "ok", "kernel exposes seccomp user notification", "")
	}
	add("PERMISSIONS", "warn", "Kernel support does not prove permission for pidfd_getfd, namespaces or every fault mode.", "Run a small representative spec under the same user as CI. Elevate only for the capabilities that spec needs.")
	if o.Docker || o.Trace {
		out, err := p.command(ctx, "docker", "info", "--format", "{{.OSType}}")
		if err != nil || strings.TrimSpace(string(out)) != "linux" {
			add("DOCKER", "error", "Linux Docker daemon is not reachable by the current user", "Start Docker and check access with `docker info`; doctor never starts or reconfigures the daemon.")
		} else {
			add("DOCKER", "ok", "Linux Docker daemon reachable", "")
		}
		shim := filepath.Join(filepath.Dir(o.Executable), "faultbox-shim")
		info, err := p.stat(shim)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			add("SHIM", "error", "matching executable faultbox-shim not found beside faultbox", "Install the complete Linux release archive, including faultbox-shim, into the same directory.")
		} else {
			add("SHIM", "ok", "executable shim at "+shim, "")
			checkShim(&r, o.Executable, shim, p.arch)
		}
	}
	if o.Packet {
		if _, err := p.stat("/dev/net/tun"); err != nil {
			add("TUN", "error", "/dev/net/tun unavailable", "Enable TUN in the Linux runner.")
		} else {
			add("TUN", "ok", "TUN device present", "")
		}
		status, _ := p.read("/proc/self/status")
		if !hasNetAdmin(string(status)) {
			add("CAP_NET_ADMIN", "error", "current process lacks CAP_NET_ADMIN", "Run packet-fault specs with CAP_NET_ADMIN (for example sudo inside a dedicated VM).")
		} else {
			add("CAP_NET_ADMIN", "ok", "current process has CAP_NET_ADMIN", "")
		}
	}
	if o.Trace {
		if _, err := p.command(ctx, "runsc", "--version"); err != nil {
			add("RUNSC", "error", "runsc is not available", "Install runsc for filesystem observation; packet faults alone do not need it.")
		} else {
			add("RUNSC", "ok", "runsc available", "")
		}
		if _, err := p.command(ctx, o.Executable, "setup-trace", "--check"); err != nil {
			add("TRACE_REGISTRATION", "error", "trace host registration check failed", "Inspect `faultbox setup-trace --check`; registration and any Docker restart are separate explicit operations.")
		} else {
			add("TRACE_REGISTRATION", "ok", "trace host registration is current", "")
		}
	}
	return r
}
func hasNetAdmin(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			var caps uint64
			_, err := fmt.Sscanf(line, "CapEff:\t%x", &caps)
			return err == nil && caps&(1<<12) != 0
		}
	}
	return false
}
func checkShim(r *Report, binary, shim, arch string) {
	read := func(path string) (map[string]string, error) {
		info, err := buildinfo.ReadFile(path)
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		for _, s := range info.Settings {
			m[s.Key] = s.Value
		}
		return m, nil
	}
	main, err1 := read(binary)
	other, err2 := read(shim)
	if err1 != nil || err2 != nil {
		r.Checks = append(r.Checks, Check{"SHIM_BUILD", "warn", "cannot verify shim build identity", "Reinstall both binaries from the same release archive."})
		return
	}
	if other["GOOS"] != "linux" || other["GOARCH"] != arch {
		r.Checks = append(r.Checks, Check{"SHIM_BUILD", "error", "shim architecture does not match this runner", "Install the correct Linux platform archive."})
		return
	}
	a, b := main["vcs.revision"], other["vcs.revision"]
	if a == "" || b == "" || main["vcs.modified"] == "true" || other["vcs.modified"] == "true" {
		r.Checks = append(r.Checks, Check{"SHIM_BUILD", "warn", "clean revision identity unavailable for one or both binaries", "Use the same official release archive for verifiable shim pairing."})
	} else if a != b {
		r.Checks = append(r.Checks, Check{"SHIM_BUILD", "error", "faultbox and shim have different source revisions", "Reinstall both binaries from the same release archive."})
	} else {
		r.Checks = append(r.Checks, Check{"SHIM_BUILD", "ok", "faultbox and shim match source revision " + a, ""})
	}
}
