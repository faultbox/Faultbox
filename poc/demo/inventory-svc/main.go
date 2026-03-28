// inventory-svc — TCP service managing product stock with a WAL.
//
// Protocol (text, newline-delimited):
//   PING              → PONG
//   CHECK <sku>       → <qty> or NOT_FOUND
//   RESERVE <sku> <n> → OK <remaining> or ERR <reason>
//
// Data is stored in memory with a write-ahead log (WAL) at $WAL_PATH.
// Every RESERVE opens the WAL, appends, fsyncs, and closes — so each
// reservation is a distinct open→write→fsync→close sequence visible
// at the syscall level.
package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type inventory struct {
	mu      sync.Mutex
	stock   map[string]int
	walPath string
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "5432"
	}
	walPath := os.Getenv("WAL_PATH")
	if walPath == "" {
		walPath = "/tmp/inventory.wal"
	}

	inv := &inventory{
		stock:   make(map[string]int),
		walPath: walPath,
	}

	// Seed some stock.
	inv.seed("widget", 100)
	inv.seed("gadget", 50)
	inv.seed("gizmo", 25)

	lc := net.ListenConfig{
		Control: setReuseAddr,
	}
	ln, err := lc.Listen(context.Background(), "tcp", ":"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inventory-svc: listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "inventory-svc: listening on :%s (wal=%s)\n", port, walPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "inventory-svc: accept: %v\n", err)
			continue
		}
		go inv.handleConn(conn)
	}
}

func (inv *inventory) seed(sku string, qty int) {
	inv.mu.Lock()
	inv.stock[sku] = qty
	inv.mu.Unlock()
}

func (inv *inventory) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.SplitN(line, " ", 3)
		cmd := strings.ToUpper(parts[0])

		switch cmd {
		case "PING":
			fmt.Fprintln(conn, "PONG")

		case "CHECK":
			if len(parts) < 2 {
				fmt.Fprintln(conn, "ERR usage: CHECK <sku>")
				continue
			}
			sku := parts[1]
			inv.mu.Lock()
			qty, ok := inv.stock[sku]
			inv.mu.Unlock()
			if !ok {
				fmt.Fprintln(conn, "NOT_FOUND")
			} else {
				fmt.Fprintln(conn, qty)
			}

		case "RESERVE":
			if len(parts) < 3 {
				fmt.Fprintln(conn, "ERR usage: RESERVE <sku> <qty>")
				continue
			}
			sku := parts[1]
			qty, err := strconv.Atoi(parts[2])
			if err != nil {
				fmt.Fprintf(conn, "ERR bad qty: %v\n", err)
				continue
			}
			result := inv.reserve(sku, qty)
			fmt.Fprintln(conn, result)

		case "QUIT":
			return

		default:
			fmt.Fprintf(conn, "ERR unknown: %s\n", cmd)
		}
	}
}

func (inv *inventory) reserve(sku string, qty int) string {
	inv.mu.Lock()
	defer inv.mu.Unlock()

	current, ok := inv.stock[sku]
	if !ok {
		return "ERR not_found"
	}
	if current < qty {
		return fmt.Sprintf("ERR insufficient_stock %d", current)
	}

	// Write to WAL: open, write, fsync, close — each is a visible syscall.
	if err := inv.walAppend(fmt.Sprintf("RESERVE %s %d\n", sku, qty)); err != nil {
		return fmt.Sprintf("ERR wal: %v", err)
	}

	// Update in-memory state only after WAL succeeds.
	inv.stock[sku] = current - qty
	return fmt.Sprintf("OK %d", inv.stock[sku])
}

// walAppend opens the WAL, appends data, fsyncs, and closes.
func (inv *inventory) walAppend(entry string) error {
	f, err := os.OpenFile(inv.walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}

func setReuseAddr(network, address string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /* SO_REUSEPORT */, 1)
	})
}
