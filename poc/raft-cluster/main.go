// Command raft-node is a single node of a 3-node hashicorp/raft cluster,
// wired up as a Faultbox system-under-test.
//
// It mirrors the "chain of blocks" workload from Antithesis' Raft study
// (https://antithesis.com/blog/2026/finding-bugs-in-raft-implementations/):
// the state machine folds every applied command into a running SHA-256
// chain, so the pair (count, hash) is a compact fingerprint of the entire
// committed prefix. Two nodes that ever apply a different command at the
// same index diverge on the hash and never re-converge — which makes
// state-machine-safety violations trivially observable from outside.
//
// Faultbox integration:
//   - Peer addresses come from the auto-injected FAULTBOX_<SVC>_<IFACE>_ADDR
//     env vars, so peers dial the Faultbox tcp proxy rather than each other
//     directly. This is what puts Faultbox in the data path.
//   - Every FSM apply and every leadership observation is written to stdout
//     as a single JSON line, for consumption by observe=stdout(decoder=json).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
)

// emit writes one structured event to stdout. Faultbox's stdout event
// source decodes these with the json decoder; the "event" field becomes
// the event type used by match.event(type=...) in specs.
func emit(fields map[string]any) {
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	os.Stdout.Write(append(b, '\n'))
}

// chainFSM is the chain-of-blocks state machine. state = (count, hash)
// where hash_n = SHA256(hash_{n-1} || cmd_n).
type chainFSM struct {
	mu     sync.Mutex
	nodeID string
	count  uint64
	hash   [32]byte
	last   uint64 // index of the most recently applied log entry
}

func (f *chainFSM) Apply(l *raft.Log) any {
	f.mu.Lock()
	defer f.mu.Unlock()

	h := sha256.New()
	h.Write(f.hash[:])
	h.Write(l.Data)
	h.Sum(f.hash[:0])
	f.count++
	f.last = l.Index

	emit(map[string]any{
		"event": "fsm.apply",
		"node":  f.nodeID,
		"index": l.Index,
		"term":  l.Term,
		"count": f.count,
		"hash":  hex.EncodeToString(f.hash[:]),
		"cmd":   string(l.Data),
	})
	return nil
}

// snapshot is the serialized form of chainFSM.
type snapshot struct {
	Count uint64 `json:"count"`
	Hash  string `json:"hash"`
	Last  uint64 `json:"last"`
}

func (f *chainFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{snapshot{Count: f.count, Hash: hex.EncodeToString(f.hash[:]), Last: f.last}}, nil
}

func (f *chainFSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var s snapshot
	if err := json.NewDecoder(rc).Decode(&s); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	raw, err := hex.DecodeString(s.Hash)
	if err != nil {
		return fmt.Errorf("decode snapshot hash: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.count = s.Count
	f.last = s.Last
	copy(f.hash[:], raw)

	emit(map[string]any{
		"event": "fsm.restore",
		"node":  f.nodeID,
		"count": f.count,
		"hash":  s.Hash,
		"index": f.last,
	})
	return nil
}

// state returns the externally-visible fingerprint of the state machine.
func (f *chainFSM) state() snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return snapshot{Count: f.count, Hash: hex.EncodeToString(f.hash[:]), Last: f.last}
}

type fsmSnapshot struct{ s snapshot }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.s); err != nil {
		sink.Cancel()
		return fmt.Errorf("persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// peerAddr resolves the cluster address of node id from the env vars
// Faultbox injects for every service interface. Falling back to the raw
// FAULTBOX_* lookup (rather than a spec-supplied PEERS list) is what lets
// three mutually-referencing peers be declared in Starlark, where forward
// references between service() calls are impossible.
func peerAddr(id string) string {
	key := fmt.Sprintf("FAULTBOX_%s_RAFT_ADDR", strings.ToUpper(id))
	if v := os.Getenv(key); v != "" {
		return v
	}
	return ""
}

func main() {
	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		log.Fatal("NODE_ID is required")
	}
	peerIDs := strings.Split(os.Getenv("PEER_IDS"), ",")
	if len(peerIDs) == 0 || peerIDs[0] == "" {
		log.Fatal("PEER_IDS is required (comma-separated node ids, including self)")
	}

	bindAddr := os.Getenv("RAFT_BIND")
	if bindAddr == "" {
		log.Fatal("RAFT_BIND is required")
	}
	httpAddr := os.Getenv("HTTP_BIND")
	if httpAddr == "" {
		log.Fatal("HTTP_BIND is required")
	}

	// The address peers use to reach us. When Faultbox has pre-started a
	// tcp proxy for our raft interface this points at the proxy, putting
	// Faultbox on every peer link.
	advertise := peerAddr(nodeID)
	if advertise == "" {
		advertise = bindAddr
	}
	advertiseTCP, err := net.ResolveTCPAddr("tcp", advertise)
	if err != nil {
		log.Fatalf("resolve advertise addr %q: %v", advertise, err)
	}

	dataDir, err := os.MkdirTemp("", "raft-"+nodeID+"-")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}

	fsm := &chainFSM{nodeID: nodeID}

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(nodeID)
	// Aggressive timings: the interesting races live in the window between
	// a heartbeat landing and the main loop noticing, so short timeouts
	// mean more elections per unit of wall-clock.
	cfg.HeartbeatTimeout = 300 * time.Millisecond
	cfg.ElectionTimeout = 300 * time.Millisecond
	cfg.LeaderLeaseTimeout = 150 * time.Millisecond
	cfg.CommitTimeout = 20 * time.Millisecond
	// Snapshot early and often so InstallSnapshot paths get exercised.
	cfg.SnapshotThreshold = 16
	cfg.SnapshotInterval = 2 * time.Second
	cfg.TrailingLogs = 4
	cfg.Logger = hclog.New(&hclog.LoggerOptions{
		Name:       nodeID,
		Level:      hclog.Debug,
		Output:     os.Stderr,
		JSONFormat: true,
	})

	logStore := raft.NewInmemStore()
	stableStore := raft.NewInmemStore()
	snaps, err := raft.NewFileSnapshotStore(dataDir, 3, os.Stderr)
	if err != nil {
		log.Fatalf("snapshot store: %v", err)
	}

	transport, err := raft.NewTCPTransport(bindAddr, advertiseTCP, 5, 2*time.Second, os.Stderr)
	if err != nil {
		log.Fatalf("tcp transport on %s (advertise %s): %v", bindAddr, advertise, err)
	}

	r, err := raft.NewRaft(cfg, fsm, logStore, stableStore, snaps, transport)
	if err != nil {
		log.Fatalf("new raft: %v", err)
	}

	// Only the designated bootstrapper writes the initial configuration.
	// This matters under Faultbox: proxy addresses are only visible to
	// services that start *after* the proxied peer, so different nodes can
	// hold different views of the same peer's address. Electing one node to
	// own the configuration makes the cluster's addressing single-sourced.
	servers := make([]raft.Server, 0, len(peerIDs))
	for _, id := range peerIDs {
		addr := peerAddr(id)
		if addr == "" {
			log.Fatalf("no address for peer %q (expected FAULTBOX_%s_RAFT_ADDR)", id, strings.ToUpper(id))
		}
		servers = append(servers, raft.Server{
			Suffrage: raft.Voter,
			ID:       raft.ServerID(id),
			Address:  raft.ServerAddress(addr),
		})
	}
	if os.Getenv("BOOTSTRAP") == "1" {
		bootErr := r.BootstrapCluster(raft.Configuration{Servers: servers}).Error()
		peerView := make([]string, 0, len(servers))
		for _, s := range servers {
			peerView = append(peerView, string(s.ID)+"="+string(s.Address))
		}
		emit(map[string]any{
			"event":     "raft.bootstrap",
			"node":      nodeID,
			"advertise": advertise,
			"peers":     strings.Join(peerView, ","),
			"error":     errString(bootErr),
		})
	}

	// Surface leadership changes as trace events so specs can assert on
	// election safety (at most one leader per term) without scraping logs.
	obsCh := make(chan raft.Observation, 64)
	r.RegisterObserver(raft.NewObserver(obsCh, false, func(o *raft.Observation) bool {
		switch o.Data.(type) {
		case raft.LeaderObservation, raft.RaftState:
			return true
		}
		return false
	}))
	go func() {
		for o := range obsCh {
			switch d := o.Data.(type) {
			case raft.LeaderObservation:
				kind := "raft.leader_lost"
				if d.LeaderID != "" {
					kind = "raft.leader_elected"
				}
				emit(map[string]any{
					"event":     kind,
					"node":      nodeID,
					"leader_id": string(d.LeaderID),
					"term":      r.CurrentTerm(),
					"state":     r.State().String(),
				})
			case raft.RaftState:
				emit(map[string]any{
					"event": "raft.state",
					"node":  nodeID,
					"state": d.String(),
					"term":  r.CurrentTerm(),
				})
			}
		}
	}()

	mux := http.NewServeMux()

	// POST /apply — submit one command to the replicated log.
	mux.HandleFunc("/apply", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		f := r.Apply(body, 3*time.Second)
		if err := f.Error(); err != nil {
			emit(map[string]any{
				"event": "apply.rejected",
				"node":  nodeID,
				"cmd":   string(body),
				"error": err.Error(),
			})
			// 503: not the leader / no quorum / timed out. The client is
			// expected to retry elsewhere, exactly like a real Raft client.
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"index": f.Index()})
	})

	// GET /state — the chain fingerprint. Comparing this across nodes is
	// the state-machine-safety check.
	mux.HandleFunc("/state", func(w http.ResponseWriter, req *http.Request) {
		s := fsm.state()
		writeJSON(w, http.StatusOK, map[string]any{
			"node":          nodeID,
			"count":         s.Count,
			"hash":          s.Hash,
			"last_index":    s.Last,
			"applied_index": r.AppliedIndex(),
			"commit_index":  r.CommitIndex(),
			"state":         r.State().String(),
			"term":          r.CurrentTerm(),
		})
	})

	// GET /status — who does this node think the leader is?
	mux.HandleFunc("/status", func(w http.ResponseWriter, req *http.Request) {
		leaderAddr, leaderID := r.LeaderWithID()
		writeJSON(w, http.StatusOK, map[string]any{
			"node":         nodeID,
			"state":        r.State().String(),
			"term":         r.CurrentTerm(),
			"leader_id":    string(leaderID),
			"leader_addr":  string(leaderAddr),
			"last_index":   r.LastIndex(),
			"commit_index": r.CommitIndex(),
		})
	})

	// POST /transfer — leadership transfer, the Consul extension implicated
	// in the Antithesis "deadlock after leadership transfer" finding.
	mux.HandleFunc("/transfer", func(w http.ResponseWriter, req *http.Request) {
		err := r.LeadershipTransfer().Error()
		emit(map[string]any{"event": "raft.transfer", "node": nodeID, "error": errString(err)})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// POST /snapshot — force a snapshot, to drive InstallSnapshot on peers.
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, req *http.Request) {
		err := r.Snapshot().Error()
		emit(map[string]any{"event": "raft.snapshot", "node": nodeID, "error": errString(err)})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// GET /wait_leader — block until this node knows of a leader, or the
	// deadline passes. Starlark has no sleep, so the wait has to live on
	// the SUT side; without it the workload races cluster formation.
	mux.HandleFunc("/wait_leader", func(w http.ResponseWriter, req *http.Request) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if addr, id := r.LeaderWithID(); addr != "" {
				writeJSON(w, http.StatusOK, map[string]any{
					"leader_id": string(id), "leader_addr": string(addr), "term": r.CurrentTerm(),
				})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no leader within deadline"})
	})

	// GET /health — used by the Faultbox healthcheck. Deliberately does not
	// require a leader: the node is "up" as soon as it serves HTTP.
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": nodeID})
	})

	emit(map[string]any{
		"event":     "node.started",
		"node":      nodeID,
		"bind":      bindAddr,
		"advertise": advertise,
		"http":      httpAddr,
	})

	srv := &http.Server{Addr: httpAddr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
