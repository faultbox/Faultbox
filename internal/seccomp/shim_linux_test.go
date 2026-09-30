//go:build linux

package seccomp

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseShimSignal covers the child→parent pipe protocol, including
// the "ERR <message>" shape added for F-2 (v0.13.0 eval): a missing
// target binary must fail the launch immediately with the root cause
// instead of surfacing as a healthcheck timeout a minute later.
func TestParseShimSignal(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantFd  int
		wantErr string // substring; empty = no error
	}{
		{"no filter", "0\n", 0, ""},
		{"listener fd", "5\n", 5, ""},
		{"exec enoent", "ERR exec /tmp/order-service-bin: no such file or directory\n", -1,
			"launch target: exec /tmp/order-service-bin: no such file or directory"},
		{"garbage", "bogus\n", -1, `parse listener fd "bogus"`},
		{"empty", "\n", -1, "parse listener fd"},
	}
	for _, tc := range cases {
		fd, err := parseShimSignal(tc.raw)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			if fd != tc.wantFd {
				t.Errorf("%s: fd = %d, want %d", tc.name, fd, tc.wantFd)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want substring %q", tc.name, err, tc.wantErr)
		}
	}
}

// The test executable serves as the re-exec shim just as cmd/faultbox does.
func init() {
	if IsShimChild() {
		if err := RunShimChild(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestLaunchBeforeExecPrecedesFastTarget(t *testing.T) {
	dir := t.TempDir()
	registered, output := filepath.Join(dir, "registered"), filepath.Join(dir, "target")
	called := 0
	pid, fd, err := Launch(LaunchConfig{
		TargetBinary: "/bin/sh", TargetArgs: []string{"-c", `test -f "$1" || exit 77; printf started > "$2"`, "target", registered, output},
		BeforeExec: func(pid int) error {
			called++
			executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if executable != self {
				return fmt.Errorf("target exec preceded registration: %s", executable)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				return fmt.Errorf("target already acted: %v", err)
			}
			return os.WriteFile(registered, []byte(strconv.Itoa(pid)), 0600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fd != -1 || called != 1 {
		t.Fatalf("fd=%d calls=%d", fd, called)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("target status=%v", status)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "started" {
		t.Fatalf("output=%q err=%v", data, err)
	}
}

func TestLaunchRegistrationFailureReapsChild(t *testing.T) {
	output := filepath.Join(t.TempDir(), "never")
	cause := errors.New("registration rejected")
	var registeredPID int
	pid, fd, err := Launch(LaunchConfig{TargetBinary: "/bin/sh", TargetArgs: []string{"-c", `printf started > "$1"`, "target", output}, BeforeExec: func(pid int) error { registeredPID = pid; return cause }})
	if !errors.Is(err, cause) || pid != registeredPID || pid <= 0 || fd != -1 {
		t.Fatalf("pid=%d registered=%d fd=%d err=%v", pid, registeredPID, fd, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("target executed: %v", err)
	}
	if _, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); err != unix.ECHILD {
		t.Fatalf("child not reaped: %v", err)
	}
	if err := unix.Kill(pid, 0); err != unix.ESRCH {
		t.Fatalf("child still exists: %v", err)
	}
}

func TestLaunchRegistrationThenInvalidTargetReapsChild(t *testing.T) {
	called := 0
	pid, _, err := Launch(LaunchConfig{TargetBinary: filepath.Join(t.TempDir(), "missing"), BeforeExec: func(int) error { called++; return nil }})
	if err == nil || called != 1 || pid <= 0 {
		t.Fatalf("pid=%d calls=%d err=%v", pid, called, err)
	}
	if _, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); err != unix.ECHILD {
		t.Fatalf("child not reaped: %v", err)
	}
}

func TestShimBrokenExecGateNeverExecutes(t *testing.T) {
	for _, test := range []struct {
		name   string
		permit []byte
	}{{"closed", nil}, {"invalid", []byte{0}}} {
		t.Run(test.name, func(t *testing.T) {
			gate := []int{0, 0}
			signal := []int{0, 0}
			if err := unix.Pipe2(gate, unix.O_CLOEXEC); err != nil {
				t.Fatal(err)
			}
			if err := unix.Pipe2(signal, unix.O_CLOEXEC); err != nil {
				t.Fatal(err)
			}
			defer unix.Close(signal[0])
			if len(test.permit) > 0 {
				if _, err := unix.Write(gate[1], test.permit); err != nil {
					t.Fatal(err)
				}
			}
			unix.Close(gate[1])
			output := filepath.Join(t.TempDir(), "never")
			data, err := json.Marshal(ShimConfig{TargetBinary: "/bin/sh", TargetArgs: []string{"-c", `printf started > "$1"`, "target", output}, GateFd: gate[0], PipeFd: signal[1]})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(ShimEnvKey, string(data))
			if err := RunShimChild(); err == nil || !strings.Contains(err.Error(), "gate") {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("target executed: %v", err)
			}
			buf := make([]byte, 512)
			n, err := unix.Read(signal[0], buf)
			if err != nil || !strings.HasPrefix(string(buf[:n]), "ERR ") {
				t.Fatalf("signal=%q err=%v", buf[:n], err)
			}
		})
	}
}

func TestLaunchRejectedRegistrationClosesFDs(t *testing.T) {
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := countFDs()
	for i := 0; i < 20; i++ {
		pid, _, err := Launch(LaunchConfig{TargetBinary: "/bin/true", BeforeExec: func(int) error { return errors.New("reject") }})
		if err == nil {
			t.Fatal("registration rejection ignored")
		}
		if _, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); err != unix.ECHILD {
			t.Fatalf("unreaped child: %v", err)
		}
	}
	if after := countFDs(); after != before {
		t.Fatalf("fd leak: before=%d after=%d", before, after)
	}
}

func TestLaunchBeforeExecPreservesPIDNamespaceAndOutput(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	calledPID := 0
	pid, _, err := Launch(LaunchConfig{TargetBinary: "/bin/sh", TargetArgs: []string{"-c", `printf '%s' "$$"`}, Cloneflags: unix.CLONE_NEWPID, StdoutFd: stdout.Fd(), BeforeExec: func(pid int) error { calledPID = pid; return nil }})
	if errors.Is(err, unix.EPERM) {
		t.Skip("PID namespace capability unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	if calledPID != pid || pid <= 1 {
		t.Fatalf("callback PID=%d launched host PID=%d", calledPID, pid)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	if status.ExitStatus() != 0 {
		t.Fatalf("target status=%v", status)
	}
	data, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1" {
		t.Fatalf("child PID namespace or stdout override lost: %q", data)
	}
}

func TestLaunchWithoutRegistrationCallback(t *testing.T) {
	pid, fd, err := Launch(LaunchConfig{TargetBinary: "/bin/true"})
	if err != nil || fd != -1 {
		t.Fatalf("pid=%d fd=%d err=%v", pid, fd, err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("target status=%v", status)
	}
}
