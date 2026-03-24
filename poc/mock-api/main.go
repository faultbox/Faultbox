// Package main is a mock API service for testing Faultbox multi-service.
// It serves HTTP and talks to mock-db over TCP.
//
// Endpoints:
//   - GET  /health        → 200 OK
//   - POST /data/:key     → SET key in mock-db, returns 200 or 500
//   - GET  /data/:key     → GET key from mock-db, returns 200+value or 404
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

var dbAddr string

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbAddr = os.Getenv("DB_ADDR")
	if dbAddr == "" {
		dbAddr = "localhost:5432"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/data/", dataHandler)

	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /* SO_REUSEPORT */, 1)
			})
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", ":"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mock-api: listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("mock-api: listening on :%s (db=%s)\n", port, dbAddr)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "mock-api: %v\n", err)
		os.Exit(1)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func dataHandler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/data/")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		value := strings.TrimSpace(string(body))
		if value == "" {
			value = "default"
		}
		resp, err := dbCommand(fmt.Sprintf("SET %s %s", key, value))
		if err != nil {
			http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
			return
		}
		if resp != "OK" {
			http.Error(w, fmt.Sprintf("db error: %s", resp), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "stored: %s=%s\n", key, value)

	case http.MethodGet:
		resp, err := dbCommand(fmt.Sprintf("GET %s", key))
		if err != nil {
			http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
			return
		}
		if resp == "NOT_FOUND" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, resp)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func dbCommand(cmd string) (string, error) {
	conn, err := net.DialTimeout("tcp", dbAddr, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("connect to db: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintln(conn, cmd)

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no response from db")
}
