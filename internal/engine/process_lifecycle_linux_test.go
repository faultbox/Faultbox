//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/seccomp"
	"golang.org/x/sys/unix"
)

func init() {
	if seccomp.IsShimChild() {
		if err := seccomp.RunShimChild(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestSessionProcessRegistrationCleanup(t *testing.T) {
	for _, kind := range []string{"success", "filter", "callback_error", "missing_binary", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			registered := filepath.Join(dir, "registered")
			target := filepath.Join(dir, "target")
			var starts, cleanups atomic.Int32
			var pid int
			cause := errors.New("ownership registration rejected")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := SessionConfig{Binary: "/bin/sh", Args: []string{"-c", `test -f "$1" || exit 77; printf started > "$2"`, "target", registered, target}, ExternalListenerFd: -1}
			if kind == "filter" {
				cfg.FaultRules = []FaultRule{{Syscall: "write", Action: ActionTrace}}
			}
			if kind == "missing_binary" {
				cfg.Binary = filepath.Join(dir, "absent")
			}
			if kind == "cancel" {
				cfg.Args = []string{"-c", `test -f "$1" || exit 77; printf started > "$2"; exec sleep 60`, "target", registered, target}
			}
			cfg.OnProcessStart = func(hostPID int) (func(), error) {
				starts.Add(1)
				pid = hostPID
				cleanup := func() {
					cleanups.Add(1)
					if err := unix.Kill(hostPID, 0); err != unix.ESRCH {
						t.Errorf("cleanup preceded reaping: %v", err)
					}
				}
				if kind == "callback_error" {
					return cleanup, cause
				}
				err := os.WriteFile(registered, []byte("owned"), 0600)
				if kind == "cancel" {
					cancel()
				}
				return cleanup, err
			}
			session, err := NewSession(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.Run(ctx)
			if starts.Load() != 1 || cleanups.Load() != 1 {
				t.Fatalf("starts=%d cleanups=%d", starts.Load(), cleanups.Load())
			}
			if kind == "callback_error" && !errors.Is(err, cause) {
				t.Fatalf("err=%v", err)
			}
			if kind == "missing_binary" && err == nil {
				t.Fatal("missing target succeeded")
			}
			if kind == "success" || kind == "filter" {
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if data, err := os.ReadFile(target); err != nil || string(data) != "started" {
					t.Fatalf("target=%q err=%v", data, err)
				}
			}
			if kind == "callback_error" || kind == "missing_binary" {
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("failed launch executed target: %v", err)
				}
			}
			if _, err := unix.Wait4(pid, nil, unix.WNOHANG, nil); err != unix.ECHILD {
				t.Fatalf("child not reaped: %v", err)
			}
		})
	}
}
