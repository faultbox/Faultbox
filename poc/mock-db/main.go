// Package main is a mock database service for testing Faultbox multi-service.
// It listens on a TCP port and responds to simple text commands:
//   - "PING\n"        → "PONG\n"
//   - "SET key val\n" → "OK\n" (stores in memory)
//   - "GET key\n"     → "val\n" or "NOT_FOUND\n"
//   - "QUIT\n"        → closes connection
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
)

var (
	store   = make(map[string]string)
	storeMu sync.RWMutex
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "5432"
	}

	lc := net.ListenConfig{
		Control: setReuseAddr,
	}
	ln, err := lc.Listen(context.Background(), "tcp", ":"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mock-db: listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("mock-db: listening on :%s\n", port)

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mock-db: accept: %v\n", err)
			continue
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.SplitN(line, " ", 3)
		cmd := strings.ToUpper(parts[0])

		switch cmd {
		case "PING":
			fmt.Fprintln(conn, "PONG")
		case "SET":
			if len(parts) < 3 {
				fmt.Fprintln(conn, "ERR usage: SET key value")
				continue
			}
			storeMu.Lock()
			store[parts[1]] = parts[2]
			storeMu.Unlock()
			fmt.Fprintln(conn, "OK")
		case "GET":
			if len(parts) < 2 {
				fmt.Fprintln(conn, "ERR usage: GET key")
				continue
			}
			storeMu.RLock()
			val, ok := store[parts[1]]
			storeMu.RUnlock()
			if ok {
				fmt.Fprintln(conn, val)
			} else {
				fmt.Fprintln(conn, "NOT_FOUND")
			}
		case "QUIT":
			return
		default:
			fmt.Fprintf(conn, "ERR unknown command: %s\n", cmd)
		}
	}
}

func setReuseAddr(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /* SO_REUSEPORT */, 1)
	})
}
