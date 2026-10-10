package engine

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSessionWaitsForThreadGroupNotZombieLeader(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler needed for pthread lifecycle fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "leader-exit")
	if out, err := exec.Command(cc, "-pthread", "-o", binary, "testdata/leader_exit.c").CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, out)
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelled), func(t *testing.T) {
			dir := t.TempDir()
			ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pidCh := make(chan int, 1)
			session, err := NewSession(SessionConfig{Binary: binary, Args: []string{ready, release}, ExternalListenerFd: -1, OnProcessStart: func(pid int) (func(), error) { pidCh <- pid; return func() {}, nil }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				r   *Result
				err error
			}
			done := make(chan outcome, 1)
			go func() { r, err := session.Run(ctx); done <- outcome{r, err} }()
			pid := <-pidCh
			pidfd, err := unix.PidfdOpen(pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); _ = unix.Close(pidfd) }()
			var port string
			for {
				data, _ := os.ReadFile(ready)
				status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
				if len(data) > 0 && strings.Contains(string(status), "State:\tZ") {
					port = string(data)
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("fixture did not reach live-worker/zombie-leader state")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if _, err := strconv.Atoi(port); err != nil {
				t.Fatal(port, err)
			}
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			select {
			case result := <-done:
				t.Fatalf("reported completion while worker still owns port: %+v %v", result.r, result.err)
			case <-time.After(50 * time.Millisecond):
			}
			if cancelled {
				cancel()
			} else if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatal(result.err)
				}
				if !cancelled && result.r.ExitCode != 23 {
					t.Fatalf("exit=%d want23", result.r.ExitCode)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("process group did not finish")
			}
			ln, err := net.Listen("tcp", "127.0.0.1:"+port)
			if err != nil {
				t.Fatalf("port leaked: %v", err)
			}
			ln.Close()
		})
	}
}

func TestPIDNamespaceExitReleasesDetachedDescendantListener(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler required")
	}
	binary := filepath.Join(t.TempDir(), "tree")
	if out, err := exec.Command(cc, "-pthread", "-o", binary, "testdata/leader_exit.c").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			dir := t.TempDir()
			ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := SessionConfig{Binary: binary, Args: []string{ready, release, "fork"}, ExternalListenerFd: -1, Namespaces: NamespaceConfig{PID: true, User: true}}
			session, err := NewSession(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan *Result, 1)
			errs := make(chan error, 1)
			go func() { r, err := session.Run(ctx); result <- r; errs <- err }()
			var port string
			for port == "" {
				data, _ := os.ReadFile(ready)
				port = string(data)
				if port != "" {
					break
				}
				select {
				case err := <-errs:
					if errors.Is(err, unix.EPERM) {
						t.Skip("PID/user namespaces unavailable")
					}
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal("descendant never opened port")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if cancelled {
				cancel()
			} else if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-errs:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("PID namespace did not stop")
			}
			r := <-result
			if !cancelled && r.ExitCode != 23 {
				t.Fatal(r)
			}
			ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
			if err != nil {
				t.Fatal("detached descendant leaked listener", err)
			}
			ln.Close()
		})
	}
}
