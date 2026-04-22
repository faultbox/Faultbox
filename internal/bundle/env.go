package bundle

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// GatherEnv collects the environment fingerprint that goes into the
// bundle's env.json. Every piece is best-effort: if docker isn't on
// PATH we omit its version rather than failing the bundle write.
// Callers (cmd/faultbox) pass in the Faultbox-specific fields because
// those come from compile-time variables that only the main package
// has access to.
func GatherEnv(faultboxVersion, faultboxCommit string) Env {
	env := Env{
		FaultboxVersion: faultboxVersion,
		FaultboxCommit:  faultboxCommit,
		HostOS:          runtime.GOOS,
		HostArch:        runtime.GOARCH,
		GoToolchain:     runtime.Version(),
	}

	if kernel := runUname("-r"); kernel != "" {
		env.Kernel = kernel
	}
	if ver := dockerVersion(); ver != "" {
		env.DockerVersion = ver
	}
	if hints := detectRuntimeHints(); len(hints) > 0 {
		env.RuntimeHints = hints
	}

	return env
}

// runUname runs `uname <flag>` and returns its trimmed output, or "" if
// the command isn't available or fails. Linux-only in practice; on
// other platforms `uname` is optional and we simply skip the kernel
// field.
func runUname(flag string) string {
	out, err := exec.Command("uname", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerVersion returns the short version string emitted by
// `docker --version`, or "" if the docker CLI isn't on PATH.
// We don't shell out to `docker info` (slower, and we don't need the
// daemon version for fingerprinting).
func dockerVersion() string {
	out, err := exec.Command("docker", "--version").Output()
	if err != nil {
		return ""
	}
	// "Docker version 27.3.1, build abc1234" -> "27.3.1"
	s := strings.TrimSpace(string(out))
	const prefix = "Docker version "
	if !strings.HasPrefix(s, prefix) {
		return s
	}
	rest := s[len(prefix):]
	if comma := strings.IndexByte(rest, ','); comma >= 0 {
		return rest[:comma]
	}
	return rest
}

// detectRuntimeHints labels the environment with tags that help
// debuggers attribute "works on my machine" differences. We look for
// Lima (macOS dev flow) and WSL (Windows dev flow) — both leave
// well-known marker files.
func detectRuntimeHints() []string {
	var hints []string
	if _, err := os.Stat("/mnt/lima-cidata"); err == nil {
		hints = append(hints, "lima")
	}
	// WSL writes a release string containing "microsoft" in /proc/version.
	if data, err := os.ReadFile("/proc/version"); err == nil {
		if strings.Contains(strings.ToLower(string(data)), "microsoft") {
			hints = append(hints, "wsl")
		}
	}
	return hints
}
