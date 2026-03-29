// api-svc is a simple HTTP API that uses Postgres and Redis.
// Designed for the Faultbox container demo (PoC 2).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:test@localhost:5432/testdb?sslmode=disable"
	}
	redisURL := os.Getenv("REDIS_URL")
	_ = redisURL // Redis integration is placeholder for now

	// Connect to Postgres with retries.
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
			err = db.Ping()
		}
		if err == nil {
			break
		}
		log.Printf("waiting for postgres: %v", err)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer db.Close()

	// Create table.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT)`)
	if err != nil {
		log.Fatalf("failed to create table: %v", err)
	}

	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/data", handleData)
	http.HandleFunc("/data/", handleDataGet)

	log.Printf("api-svc: listening on :%s (db=%s)", port, dbURL)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := db.PingContext(r.Context()); err != nil {
		http.Error(w, "db unhealthy: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func handleData(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handleDataPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleDataPost(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	value := r.URL.Query().Get("value")
	if key == "" || value == "" {
		http.Error(w, "key and value required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = $2`,
		key, value)
	if err != nil {
		http.Error(w, "db write failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "stored: %s=%s", key, value)
}

func handleDataGet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path[len("/data/"):]
	if key == "" {
		http.Error(w, "key required in path", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = $1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db read failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, value)
}
