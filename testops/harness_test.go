package testops

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var updateGoldens = flag.Bool("update", false, "regenerate goldens/*.norm from current run output")

// goldensDir is the on-disk location of committed golden files.
const goldensDir = "goldens"

// defaultTimeout applies when a Case leaves Timeout zero.
const defaultTimeout = 60 * time.Second

func TestGoldens(t *testing.T) {
	root := repoRoot(t)
	bin := ensureFaultboxBinary(t, root)

	for _, c := range Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Skip != "" {
				t.Skip(c.Skip)
			}
			if c.LinuxOnly && runtime.GOOS != "linux" {
				t.Skipf("LinuxOnly case; current GOOS=%s", runtime.GOOS)
			}
			runGoldenCase(t, root, bin, c)
		})
	}
}

func runGoldenCase(t *testing.T, root, bin string, c Case) {
	t.Helper()

	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	specPath := filepath.Join(root, c.Spec)
	if _, err := os.Stat(specPath); err != nil {
		t.Fatalf("spec %s: %v", c.Spec, err)
	}

	outFile := filepath.Join(t.TempDir(), c.Name+".norm")

	cmd := exec.Command(bin,
		"test", specPath,
		"--seed", fmt.Sprintf("%d", c.Seed),
		"--normalize", outFile,
		"--format", "json",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "NO_COLOR=1")

	done := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) > 0 {
			t.Logf("faultbox output:\n%s", out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("faultbox test failed: %v", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("faultbox test timed out after %s", timeout)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read normalized trace: %v", err)
	}

	goldenPath := filepath.Join(root, "testops", goldensDir, c.Name+".norm")

	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir goldens: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d bytes)", goldenPath, len(got))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("no golden at %s; run `go test ./testops/... -run %s -update` to seed it",
				goldenPath, c.Name)
		}
		t.Fatalf("read golden: %v", err)
	}

	if string(got) == string(want) {
		return
	}

	// Use `faultbox diff` for a readable, colorized comparison.
	diff, derr := exec.Command(bin, "diff", goldenPath, outFile).CombinedOutput()
	if derr != nil {
		t.Logf("faultbox diff failed (%v); raw trace follows", derr)
		t.Fatalf("golden mismatch:\n--- want (%s) ---\n%s\n--- got ---\n%s",
			goldenPath, truncate(string(want), 2000), truncate(string(got), 2000))
	}
	t.Fatalf("golden mismatch for %s:\n%s\n\nTo accept: go test ./testops/... -run %s -update",
		c.Name, diff, c.Name)
}

// repoRoot walks up from the test binary's working directory until it
// finds the go.mod, which is the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	d := wd
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("could not find go.mod from %s", wd)
		}
		d = parent
	}
}

// ensureFaultboxBinary returns a path to ./bin/faultbox, building it if
// missing. Always using the in-repo build avoids accidentally testing
// the wrong binary from $PATH.
func ensureFaultboxBinary(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "bin", "faultbox")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/faultbox")
	build.Dir = root
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build faultbox: %v\n%s", err, out)
	}
	return bin
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated, " + fmt.Sprintf("%d", len(s)-n) + " bytes] ...\n"
}
