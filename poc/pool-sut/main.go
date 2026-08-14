// pool-sut is the regression target for F-1: a SUT that generates enough
// concurrent intercepted syscalls to reproduce the dropped-notification
// race that used to kill the seccomp supervisor.
//
// The corpus previously had no service like this. Every existing poc
// binary is effectively single-threaded and quiet, and the bug needs
// volume: ENOENT from SECCOMP_IOCTL_NOTIF_RECV happens when a notifying
// thread is interrupted before the supervisor receives its notification,
// so the probability scales with how many threads are in a syscall at
// once. A busy Go service hits it in seconds; a quiet demo essentially
// never does. That gap is why the defect shipped.
//
// It holds a pool of TCP connections to an upstream, keeps every one of
// them doing round trips, and exposes:
//
//	GET /health   200 once the pool is established
//	GET /stats    JSON: completed round trips, errors, stalls
//
// A stall is a round trip that exceeded stallThreshold. Under the old
// behaviour, every in-flight request stalls permanently the moment the
// supervisor dies, so a non-zero stall count is the regression signal.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const stallThreshold = 5 * time.Second

type stats struct {
	roundTrips atomic.Int64
	errors     atomic.Int64
	stalls     atomic.Int64
	maxRTTMs   atomic.Int64
}

func main() {
	port := envOr("PORT", "8080")
	upstream := envOr("UPSTREAM_ADDR", "127.0.0.1:5432")
	poolSize := envIntOr("POOL_SIZE", 24)

	var st stats
	var ready sync.WaitGroup
	ready.Add(poolSize)

	for i := 0; i < poolSize; i++ {
		go worker(i, upstream, &st, &ready)
	}

	// Wait for the pool to establish before reporting healthy, so the
	// healthcheck cannot pass on a SUT that has not opened a socket yet.
	done := make(chan struct{})
	go func() { ready.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Printf("pool did not fully establish within 30s; serving anyway")
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{
			"round_trips": st.roundTrips.Load(),
			"errors":      st.errors.Load(),
			"stalls":      st.stalls.Load(),
			"max_rtt_ms":  st.maxRTTMs.Load(),
		})
	})

	log.Printf("pool-sut listening on :%s, pool=%d upstream=%s", port, poolSize, upstream)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// worker keeps one connection busy. On any error it reconnects, so a
// supervisor that stops answering shows up as stalled round trips rather
// than as a worker that quietly gives up.
func worker(id int, upstream string, st *stats, ready *sync.WaitGroup) {
	var conn net.Conn
	var err error
	signalled := false

	for {
		if conn == nil {
			conn, err = net.DialTimeout("tcp", upstream, stallThreshold)
			if err != nil {
				st.errors.Add(1)
				if !signalled {
					ready.Done()
					signalled = true
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if !signalled {
				ready.Done()
				signalled = true
			}
		}

		start := time.Now()
		if err := roundTrip(conn, id); err != nil {
			st.errors.Add(1)
			conn.Close()
			conn = nil
			if elapsed := time.Since(start); elapsed >= stallThreshold {
				st.stalls.Add(1)
			}
			continue
		}

		elapsed := time.Since(start)
		st.roundTrips.Add(1)
		if ms := elapsed.Milliseconds(); ms > st.maxRTTMs.Load() {
			st.maxRTTMs.Store(ms)
		}
		if elapsed >= stallThreshold {
			st.stalls.Add(1)
		}
	}
}

// roundTrip issues one SET against mock-db. Both the write and the read
// are intercepted when a filter is installed, which is the point.
func roundTrip(conn net.Conn, id int) error {
	conn.SetDeadline(time.Now().Add(stallThreshold * 2))

	cmd := fmt.Sprintf("SET pool-%d %d\n", id, time.Now().UnixNano())
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return err
	}

	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err != nil {
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}
