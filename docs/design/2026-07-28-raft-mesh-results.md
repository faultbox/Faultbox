# Peer-mesh faults: what the Raft harness shows now

**Date:** 2026-07-28
**Status:** Results. Follow-up to the [gap analysis](2026-07-28-raft-antithesis-gap-analysis.md) and the [mesh proposal](2026-07-28-v0.14.0-mesh-topology-proposal.md)
**Branch:** `epic/v0.14.0-gvisor` @ `6ceb279`
**Harness:** [`poc/raft-cluster/`](../../poc/raft-cluster/) — 3-node `hashicorp/raft` **v1.7.3**, chain-of-blocks FSM
**Environment:** Lima `faultbox-dev`, kernel 6.8.0, `determinism(runtime = "gvisor")`

---

## Summary

The four proposed changes landed. The fault model now reaches the consensus
layer, which it demonstrably did not before.

**It did not reproduce the Antithesis bugs.** Across 40 randomized runs,
`hashicorp/raft` v1.7.3 behaved correctly in every scenario the harness can
now express. That is a real result and this document treats it as one, rather
than reporting a bug that is not there.

| Claim | Evidence |
|---|---|
| Packet faults reach peer links | Leader isolated from quorum: **88 applies → 0** |
| Partitions work on established connections | `failed to contact quorum of nodes, stepping down` |
| One-way partitions expressible | `partition(node1, node2, direction="a_to_b")` runs |
| Antithesis bug 2 (transfer deadlock) | **Not reproduced** — 25 runs |
| Antithesis bug 3 (snapshot livelock) | **Not reproduced** — 15 runs |
| Antithesis bug 1 (heartbeat race) | Out of scope; needs RFC-053 Path D |

---

## 1. The decisive measurement

The gap analysis's sharpest finding was that a leader cut off from **both**
followers went on committing:

```
BEFORE (main, connect-deny partition)
  isolate: applies during full isolation = 88     # 30 more commands committed
  isolate: leader_elected=3 leader_lost=0 apply_rejected=0
  --- PASS: test_isolate_leader_from_quorum ---   # passed, having injected nothing
```

Same assertion, same workload, after the four changes:

```
AFTER (epic/v0.14.0-gvisor, packet-gateway partition)
  isolate: applies while leader had no quorum = 0
  --- PASS: test_isolate_leader_from_quorum (846ms) ---
```

And the cluster says so itself:

```
[WARN] failed to contact  server-id=node2 time=150458359
[WARN] failed to contact  server-id=node3 time=150456566
[WARN] failed to contact quorum of nodes, stepping down
```

That is Raft doing exactly what Raft promises. The point is that the fault
finally arrived: the old `partition()` denied `connect()`, and peers had long
since stopped calling it.

## 2. What changed, and why each mattered

### The blocker: gateway addresses were gated on a proxy existing

`gatewayAddrFor` refused to allocate unless a *proxy* address was already
known. `preStartProxies` starts an interface's proxy when its owning service
starts, and only services launched afterwards see it — correct for a
dependency tree, impossible for a cycle. For at least one mesh link the proxy
is always absent when the consumer's env is built, so that link stayed
unmediated.

The real upstream is known at spec load. Chaining through a proxy is an
optimization, not a precondition.

The harness previously worked around this with a startup-order hack — node3 →
node2 → node1, node1 as sole bootstrapper — which still left node1's own
inbound link unmediated. The spec now has **no ordering and no `depends_on`**,
and every peer link is on the gateway:

```
packet_gateway_route  consumer=node1  raft   10.99.0.3:8301 → 127.0.0.1:8301
packet_gateway_route  consumer=node1  raft   10.99.0.5:8302 → 127.0.0.1:8302
packet_gateway_route  consumer=node1  raft   10.99.0.6:8303 → 127.0.0.1:8303
   … one address per (consumer, service, interface) triple
```

### `source=` reached rule installation

It was parsed, stored, and written into the trace — then dropped. So
`fault(kafka.main, source=worker, drop(...))`, the example in the docs, quietly
installed a rule that fired for *every* consumer.

Because the allocator already gives each triple its own address, scoping is a
destination predicate rather than packet inspection. Docker masquerades
container source IPs, so the source address could not have identified the
sender anyway.

### `partition()` moved onto the gateway

`partition_start` / `partition_stop` and `direction=` came with it. Under
`runtime="default"` it now **errors** rather than falling back to `connect`
deny: a primitive that silently does nothing against any connection-pooling
service is worse than one that refuses.

### `EventVal` gained `.data`

The documented monitor example could not run — only `StarlarkEvent`
auto-decoded, so `event.data.get("level", "")` failed with *"string has no .get
field or method"*. Both event surfaces now share one decode path.

## 3. Two more defects, found by running it

**A service's own bind address was being rewritten.** The substitution table is
a *dial*-address rewrite applied by substring match over user env, so
`RAFT_BIND="127.0.0.1:8301"` became a gateway address and the node tried to
bind an address it does not own — exit 1, cluster never formed. Only reachable
once the gateway stopped requiring a proxy. Self-owned interfaces are now
skipped.

**The documented `source=` example does not parse.** It puts `source=` ahead of
the positional fault rules, which Starlark forbids:

```python
fault(kafka.main, source=worker,      # positional argument may not follow named
    drop(topic="orders.*"),
    run=scenario,
)
```

Third documented example this release that could never have run, after
`events(where=lambda e: e.type == "proxy" ...)` and the monitor `.data` case.
All three now have guard tests.

## 4. On not finding the bugs

The harness can now express both target scenarios. Neither triggered.

```
test_transfer_during_partition   --runs 25   →  25 passed, 0 failed
test_snapshot_install_livelock   --runs 15   →  15 passed, 0 failed
```

### A false positive, caught

The first run of the snapshot scenario looked like a textbook livelock:

```
snapshot: node1 count=205  hash=124cb19b…
snapshot: node2 count=204  hash=ccb2f7ac…
snapshot: node3 count=0    hash=00000000…    ← never caught up
--- FAIL: test_snapshot_install_livelock ---
```

It was not. The whole test ran in **723 ms**, against a cluster whose
`SnapshotInterval` is 2 s. The follower had not failed to catch up; it had not
been *given time* to. Adding `await_stable(quiescence_window="1s")` between
catch-up rounds:

```
2026-07-28T20:11:21.337  snapshot: creating new snapshot  node3
snapshot: converged after round 0
snapshot: node1 count=202 hash=33acd69e…
snapshot: node2 count=202 hash=33acd69e…
snapshot: node3 count=202 hash=33acd69e…
```

node3 received the snapshot and converged. Had the assertion shipped as first
written, this document would be reporting a bug in `hashicorp/raft` that does
not exist.

The lesson generalises: **a liveness assertion needs an explicit convergence
window, or it is really an assertion about how fast the test ran.**

### Why the bugs did not appear

Two candidate explanations, and the honest answer is that this harness cannot
distinguish them:

1. **Fixed upstream.** The harness pins `hashicorp/raft` v1.7.3. If the
   Antithesis findings were reported and fixed, correct behaviour is exactly
   what we should see. Confirming this needs a run against the version their
   study used.
2. **Not enough search.** Antithesis found these through massive randomized
   exploration of interleavings and fault timings. Forty scripted runs with a
   fixed fault sequence is a very different search. Both bugs are races — bug 2
   needs the transfer goroutine to be mid-replication exactly when the leader
   steps down; bug 3 needs the follower's log in a specific state when the
   snapshot lands. A fixed script hits one point in that space per run.

Reaching them plausibly needs the fault *timing* to vary, not just the seed:
partition onset relative to the transfer call, isolation duration relative to
`SnapshotInterval`. `choose()` over those parameters with `--explore` is the
natural next step, and is not something this epic scoped.

## 5. Where each Antithesis bug now stands

| Bug | Blocker in the gap analysis | Status |
|---|---|---|
| 1 — async heartbeat race | Needs per-message hold/release attributed to a goroutine | **Still out of scope.** Packet timing influences the window; it does not control it. Needs [RFC-053](https://github.com/faultbox/Faultbox/issues/143) Path D — seccomp reports M, not G. |
| 2 — leadership-transfer deadlock | Gaps 1 and 3: partition on established connections | **Expressible now.** `partition_start` + `/transfer` + `partition_stop` runs as intended. Not triggered in 25 runs. |
| 3 — snapshot-install livelock | Gaps 1 and 3: sustained isolation of a live peer | **Expressible now.** Follower falls 200 commands behind and needs `InstallSnapshot`. Not triggered in 15 runs. |

The gap analysis's verdict was *"Faultbox can host the workload but cannot yet
break the cluster."* It can break the cluster now — the isolation test proves
it. What it has not yet done is break `hashicorp/raft`.

## 6. What is still missing

- **Fault-timing exploration.** `choose()` over partition onset and duration,
  driven by `--explore`, so the search covers the space rather than one point
  in it. This is the most likely route to bugs 2 and 3.
- **Pre-starting all proxies.** Packet faults reach every mesh link; L7 faults
  (`error()`, `response()`) still do not for links whose peer started later.
  Deferred to v0.14.1 with the caveat documented.
- **RFC-053 Path D** for bug 1. Unchanged.
- **A run against the version Antithesis studied**, to settle §4's ambiguity.

## 7. Reproducing

```bash
cd poc/raft-cluster && GOOS=linux GOARCH=arm64 go build -o /tmp/raft-node .
make env-start
limactl shell faultbox-dev
sudo faultbox test poc/raft-cluster/faultbox.star
```

```
--- PASS: test_baseline_replication        (588ms)
--- PASS: test_isolate_leader_from_quorum  (846ms)   isolate: applies with no quorum = 0
--- PASS: test_one_way_partition           (433ms)
--- PASS: test_snapshot_install_livelock  (1604ms)   converged after round 0
--- PASS: test_transfer_during_partition  (2674ms)   accepted after recovery = 10
5 passed, 0 failed
```
