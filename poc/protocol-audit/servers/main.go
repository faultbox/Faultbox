// Command servers is a real-server target for the three step protocols
// the protocol audit could not reach: http2, grpc and udp.
//
// The audit's other ten protocols talk to stock images (redis:7-alpine,
// mysql:8, …). These three had no server to talk to, so they carried unit
// coverage only — which is exactly the gap that let the MySQL and Postgres
// credential bugs survive: a plugin can look correct in isolation and
// still never complete a round trip on the wire.
//
// Stock images were not a good fit here. h2c cleartext HTTP/2 is unusual
// in published images (most terminate TLS), and a gRPC server needs a
// service definition the client also knows. So this serves all three from
// one process, using the standard implementations — grpc-go and
// x/net/http2 — so the bytes on the wire are real even though the server
// is first-party.
//
//	HTTP2_PORT  h2c (cleartext HTTP/2), routes below
//	GRPC_PORT   grpc.health.v1.Health — Check and Watch
//	UDP_PORT    echo, plus a /sink path that never replies
//
// HTTP/2 routes:
//
//	GET  /            200 "ok"
//	GET  /json        200 {"service":"protocol-audit","proto":"HTTP/2.0"}
//	POST /echo        200, body echoed back
//	GET  /status/NNN  NNN, so a spec can assert a non-2xx is surfaced
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	var wg sync.WaitGroup

	wg.Add(3)
	go func() { defer wg.Done(); serveHTTP2(envOr("HTTP2_PORT", "8443")) }()
	go func() { defer wg.Done(); serveGRPC(envOr("GRPC_PORT", "9090")) }()
	go func() { defer wg.Done(); serveUDP(envOr("UDP_PORT", "9999")) }()
	wg.Wait()
}

// serveHTTP2 runs an h2c server — HTTP/2 without TLS. The protocol
// plugin dials cleartext, so a TLS-terminating server would test the TLS
// path instead of the h2 framing this is here to exercise.
func serveHTTP2(port string) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service": "protocol-audit",
			// Echoing the negotiated protocol lets a spec prove it really
			// spoke HTTP/2 rather than falling back to 1.1.
			"proto": r.Proto,
		})
	})

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil || code < 100 || code > 599 {
			http.Error(w, "bad status", http.StatusBadRequest)
			return
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "status %d", code)
	})

	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: h2c.NewHandler(mux, h2s),
	}
	log.Printf("http2 (h2c) listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http2: %v", err)
	}
}

// serveGRPC runs the standard health service. It is the one gRPC service
// with a definition both sides already agree on, so a spec can call it
// without shipping a proto.
func serveGRPC(port string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("grpc listen: %v", err)
	}
	s := grpc.NewServer()

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("protocol-audit", healthpb.HealthCheckResponse_SERVING)
	// A second name reporting NOT_SERVING, so a spec can assert the
	// plugin surfaces a non-OK response rather than flattening it.
	hs.SetServingStatus("degraded", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(s, hs)

	log.Printf("grpc listening on :%s", port)
	if err := s.Serve(ln); err != nil {
		log.Fatalf("grpc: %v", err)
	}
}

// serveUDP echoes datagrams. A payload of "sink" is swallowed, so a spec
// can exercise send_no_reply against a server that genuinely does not
// answer rather than inferring it from a timeout.
func serveUDP(port string) {
	addr, err := net.ResolveUDPAddr("udp", ":"+port)
	if err != nil {
		log.Fatalf("udp resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatalf("udp listen: %v", err)
	}
	log.Printf("udp listening on :%s", port)

	buf := make([]byte, 64*1024)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("udp read: %v", err)
			continue
		}
		payload := buf[:n]
		if strings.TrimSpace(string(payload)) == "sink" {
			continue
		}
		if _, err := conn.WriteToUDP(payload, peer); err != nil {
			log.Printf("udp write: %v", err)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
