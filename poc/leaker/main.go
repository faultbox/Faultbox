//go:build linux

// Package main is a configurable I/O-leak harness for Faultbox's
// determinism testops corpus (RFC-040). On a /trigger?leak=<kind>
// request it performs one of the four L1-detected unmediated I/O
// patterns; the spec under test asserts the strict-determinism
// outcome the runtime produces.
//
// Build:  GOOS=linux go build -o /tmp/faultbox-leaker ./poc/leaker
// Run:    PORT=8090 /tmp/faultbox-leaker
//
// Leaks:
//   /trigger?leak=clock   → raw clock_gettime syscall (bypasses VDSO)
//   /trigger?leak=rand    → raw getrandom syscall
//   /trigger?leak=dns     → connect to 8.8.8.8:53 (best-effort; the
//                           connect itself is the signal — it doesn't
//                           need to succeed for detection to fire)
//   /trigger?leak=network → connect to 127.0.0.1:19999 (no listener
//                           there; only the connect attempt matters)
//
// Linux-only (raw syscalls + unix constants); the build tag makes
// the host's go vet on darwin / windows skip this file cleanly.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/trigger", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("leak")
		if err := performLeak(kind); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "leak=%s done\n", kind)
	})

	addr := ":" + port
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "leaker listening on %s\n", addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "leaker: %v\n", err)
		os.Exit(1)
	}
}

func performLeak(kind string) error {
	switch kind {
	case "clock":
		// Raw clock_gettime syscall — bypasses Go's VDSO fast path so
		// seccomp-notify actually sees it. CLOCK_REALTIME(0) into a
		// stack-allocated timespec.
		var ts unix.Timespec
		_, _, errno := unix.Syscall(unix.SYS_CLOCK_GETTIME, uintptr(unix.CLOCK_REALTIME), uintptr(unsafe.Pointer(&ts)), 0)
		if errno != 0 {
			return fmt.Errorf("clock_gettime: %v", errno)
		}
		return nil
	case "rand":
		// Raw getrandom syscall on a 16-byte buffer.
		buf := make([]byte, 16)
		_, _, errno := unix.Syscall(unix.SYS_GETRANDOM, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if errno != 0 {
			return fmt.Errorf("getrandom: %v", errno)
		}
		return nil
	case "dns":
		// connect() to a public DNS resolver on port 53. We don't care
		// whether it succeeds — the connect attempt itself is what
		// fires the detection. Short timeout so the test stays bounded.
		_, _ = net.DialTimeout("udp", "8.8.8.8:53", 200*time.Millisecond)
		return nil
	case "network":
		// connect() to localhost:19999 — nothing listens there in the
		// test environment, so the connect fails fast. The only thing
		// the spec asserts is that the connect happened.
		_, _ = net.DialTimeout("tcp", "127.0.0.1:19999", 200*time.Millisecond)
		return nil
	default:
		return fmt.Errorf("unknown leak kind %q (expected clock|rand|dns|network)", kind)
	}
}
