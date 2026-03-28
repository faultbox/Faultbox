// order-svc — HTTP service for placing orders.
// Talks to inventory-svc via TCP to check stock and reserve items.
//
// Endpoints:
//   GET  /health                → 200 OK
//   POST /orders                → place order (JSON body: {"sku":"widget","qty":1})
//   GET  /orders/:id            → get order status
//   GET  /inventory/:sku        → check stock level
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	inventoryAddr string
	orders        sync.Map
	orderSeq      atomic.Int64
)

type OrderRequest struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type Order struct {
	ID     int64  `json:"id"`
	SKU    string `json:"sku"`
	Qty    int    `json:"qty"`
	Status string `json:"status"` // "confirmed", "failed"
	Error  string `json:"error,omitempty"`
	Stock  int    `json:"remaining_stock,omitempty"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	inventoryAddr = os.Getenv("INVENTORY_ADDR")
	if inventoryAddr == "" {
		inventoryAddr = "localhost:5432"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/orders", ordersHandler)
	mux.HandleFunc("/orders/", orderByIDHandler)
	mux.HandleFunc("/inventory/", inventoryHandler)

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
		fmt.Fprintf(os.Stderr, "order-svc: listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "order-svc: listening on :%s (inventory=%s)\n", port, inventoryAddr)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "order-svc: %v\n", err)
		os.Exit(1)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req OrderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if req.SKU == "" {
		http.Error(w, "sku required", http.StatusBadRequest)
		return
	}
	if req.Qty <= 0 {
		req.Qty = 1
	}

	// Check stock first.
	stockResp, err := inventoryCommand(fmt.Sprintf("CHECK %s", req.SKU))
	if err != nil {
		order := failOrder(req, fmt.Sprintf("inventory unreachable: %v", err))
		writeJSON(w, http.StatusServiceUnavailable, order)
		return
	}
	if stockResp == "NOT_FOUND" {
		order := failOrder(req, "sku not found")
		writeJSON(w, http.StatusNotFound, order)
		return
	}

	// Reserve stock.
	reserveResp, err := inventoryCommand(fmt.Sprintf("RESERVE %s %d", req.SKU, req.Qty))
	if err != nil {
		order := failOrder(req, fmt.Sprintf("inventory error: %v", err))
		writeJSON(w, http.StatusServiceUnavailable, order)
		return
	}

	if !strings.HasPrefix(reserveResp, "OK") {
		order := failOrder(req, reserveResp)
		writeJSON(w, http.StatusConflict, order)
		return
	}

	// Parse remaining stock from "OK <remaining>".
	var remaining int
	fmt.Sscanf(reserveResp, "OK %d", &remaining)

	order := Order{
		ID:     orderSeq.Add(1),
		SKU:    req.SKU,
		Qty:    req.Qty,
		Status: "confirmed",
		Stock:  remaining,
	}
	orders.Store(order.ID, order)
	writeJSON(w, http.StatusOK, order)
}

func orderByIDHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/orders/")
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	val, ok := orders.Load(id)
	if !ok {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, val.(Order))
}

func inventoryHandler(w http.ResponseWriter, r *http.Request) {
	sku := strings.TrimPrefix(r.URL.Path, "/inventory/")
	if sku == "" {
		http.Error(w, "sku required", http.StatusBadRequest)
		return
	}

	resp, err := inventoryCommand(fmt.Sprintf("CHECK %s", sku))
	if err != nil {
		http.Error(w, fmt.Sprintf("inventory error: %v", err), http.StatusServiceUnavailable)
		return
	}
	if resp == "NOT_FOUND" {
		http.Error(w, "sku not found", http.StatusNotFound)
		return
	}
	fmt.Fprintf(w, `{"sku":"%s","qty":%s}`, sku, resp)
}

func failOrder(req OrderRequest, reason string) Order {
	order := Order{
		ID:     orderSeq.Add(1),
		SKU:    req.SKU,
		Qty:    req.Qty,
		Status: "failed",
		Error:  reason,
	}
	orders.Store(order.ID, order)
	return order
}

func inventoryCommand(cmd string) (string, error) {
	conn, err := net.DialTimeout("tcp", inventoryAddr, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
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
	return "", fmt.Errorf("no response from inventory")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
