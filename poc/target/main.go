// Package main is a simple target binary for testing Faultbox's control layers.
// It exercises: network (HTTP), filesystem (write/read), and syscalls (getpid).
package main

import (
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

func main() {
	fmt.Println("=== Faultbox PoC Target ===")
	fmt.Printf("PID: %d\n", syscall.Getpid())

	// Filesystem: write and read a temp file (timed for delay observation)
	path := "/tmp/faultbox-target-test"
	fsStart := time.Now()
	if err := os.WriteFile(path, []byte("hello faultbox"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "fs write failed: %v (took %s)\n", err, time.Since(fsStart))
		os.Exit(1)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fs read failed: %v (took %s)\n", err, time.Since(fsStart))
		os.Exit(1)
	}
	fmt.Printf("FS: wrote and read %d bytes (took %s)\n", len(data), time.Since(fsStart))
	os.Remove(path)

	// Network: HTTP GET (timed for delay observation)
	client := &http.Client{Timeout: 5 * time.Second}
	netStart := time.Now()
	resp, err := client.Get("http://httpbin.org/get")
	if err != nil {
		fmt.Fprintf(os.Stderr, "net failed: %v (took %s)\n", err, time.Since(netStart))
		// Don't exit — network may be intentionally blocked
	} else {
		fmt.Printf("NET: HTTP %d (took %s)\n", resp.StatusCode, time.Since(netStart))
		resp.Body.Close()
	}

	fmt.Println("=== Target done ===")
}
