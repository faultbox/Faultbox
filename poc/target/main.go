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

	// Filesystem: write and read a temp file
	path := "/tmp/faultbox-target-test"
	if err := os.WriteFile(path, []byte("hello faultbox"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "fs write failed: %v\n", err)
		os.Exit(1)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fs read failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("FS: wrote and read %d bytes\n", len(data))
	os.Remove(path)

	// Network: HTTP GET
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://httpbin.org/get")
	if err != nil {
		fmt.Fprintf(os.Stderr, "net failed: %v\n", err)
		// Don't exit — network may be intentionally blocked
	} else {
		fmt.Printf("NET: HTTP %d\n", resp.StatusCode)
		resp.Body.Close()
	}

	fmt.Println("=== Target done ===")
}
