# raft-cluster — hashicorp/raft under Faultbox

A 3-node `hashicorp/raft` v1.7.3 cluster running as a Faultbox system-under-test,
with the "chain of blocks" workload from Antithesis' Raft study:

> https://antithesis.com/blog/2026/finding-bugs-in-raft-implementations/

The state machine folds every applied command into a running SHA-256 chain, so
node state is the pair `(count, hash)`. Two nodes that ever apply a different
command at the same index diverge permanently — which makes state-machine-safety
violations observable from outside the process.

**Findings from this POC:**
[`docs/design/2026-07-28-raft-antithesis-gap-analysis.md`](../../docs/design/2026-07-28-raft-antithesis-gap-analysis.md)

## Layout

| File | What it is |
|------|-----------|
| `main.go` | One raft node. TCP transport for peers, HTTP admin API for the workload, JSON event lines on stdout. |
| `faultbox.star` | Three peer services, the state-machine-safety monitor, and the fault scenarios. |

## Running

```bash
# from the repo root — cross-compile for the Lima VM
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/linux-arm64/faultbox ./cmd/faultbox/
(cd poc/raft-cluster && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../../bin/linux-arm64/raft-node .)

limactl shell --workdir /host-home/git/faultbox/faultbox faultbox-dev -- \
  ./bin/linux-arm64/faultbox test poc/raft-cluster/faultbox.star
```

## Admin API

| Endpoint | Purpose |
|----------|---------|
| `POST /apply` | Submit one command. 200 with the log index, or 503 if this node is not the leader. |
| `GET /state` | Chain fingerprint: `count`, `hash`, applied/commit index, raft state, term. |
| `GET /status` | Who this node believes the leader is. |
| `GET /wait_leader` | Block until a leader is known (Starlark has no `sleep`). |
| `POST /transfer` | Leadership transfer — the Consul extension implicated in the transfer-deadlock bug. |
| `POST /snapshot` | Force a snapshot, to drive `InstallSnapshot` on lagging peers. |
| `GET /health` | Faultbox healthcheck. Deliberately does not require a leader. |

## Two things that are load-bearing in the spec

**Peer discovery goes through `FAULTBOX_<NODE>_RAFT_ADDR`.** Starlark evaluates
top to bottom, so `service()` calls cannot forward-reference each other — a peer
mesh is impossible to wire with `node2.raft.addr`. The auto-injected env vars are
resolved at launch instead, which sidesteps the cycle.

**Startup order determines which links Faultbox can see.** Faultbox starts an
interface's proxy when its owning service starts, and only services launched
afterwards get env vars pointing at that proxy. Starting `node3 → node2 → node1`
puts node2's and node3's raft ports behind proxies as far as node1 is concerned;
`BOOTSTRAP=1` on node1 then makes its (proxied) view of peer addresses the one
that replicates. No ordering covers every link — see the findings doc, gap 2.
