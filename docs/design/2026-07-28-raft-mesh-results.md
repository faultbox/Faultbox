# Peer-mesh faults: what the Raft harness shows now

**Date:** 2026-07-28
**Status:** Results. Follow-up to the [gap analysis](2026-07-28-raft-antithesis-gap-analysis.md) and the [mesh proposal](2026-07-28-v0.14.0-mesh-topology-proposal.md)
**Branch:** `epic/v0.14.0-gvisor` @ `6ceb279`
**Harness:** [`poc/raft-cluster/`](../../poc/raft-cluster/) — 3-node `hashicorp/raft` **main @ `4c8f61ac`**, chain-of-blocks FSM
**Environment:** Lima `faultbox-dev`, kernel 6.8.0, `determinism(runtime = "gvisor")`

---

## Summary

The four proposed changes landed. The fault model now reaches the consensus
layer, which it demonstrably did not before.

**It did not reproduce the Antithesis bugs** — including against the exact
commit their study forked from. §4 explains why, and the answer turned out to
be a defect in this harness rather than an open question about `hashicorp/raft`.

| Claim | Evidence |
|---|---|
| Packet faults reach peer links | Leader isolated from quorum: **88 applies → 0** |
| Partitions work on established connections | `failed to contact quorum of nodes, stepping down` |
| One-way partitions expressible | `partition(node1, node2, direction="a_to_b")` runs |
| Antithesis bug 2 (transfer deadlock) | **Not reproduced** — 25 runs |
| Antithesis bug 3 (snapshot livelock) | **Not reproduced** — 15 runs |
| Antithesis bug 1 (heartbeat race) | Out of scope; needs RFC-053 Path D |
| Those 40 runs explored 40 fault schedules | **False.** They explored one. §4.2 |

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

Three findings sit behind that, and the notable thing is that **none of them is
a finding about `hashicorp/raft`.** All three are defects in this harness or in
this document: a false positive caught before it shipped (§4.0), a wrong claim
about *which version* was under test (§4.1), and a wrong claim about how much of
the fault space those runs covered (§4.2).

Two of the three were introduced by the first draft of this document. They are
recorded rather than quietly corrected because the failure mode — a test that
passes for the wrong reason, described in language that sounds like evidence —
is the one this project exists to catch in other people's systems.

### 4.0 A false positive, caught

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

### 4.1 "Fixed upstream" — refuted

The first draft of this document offered two explanations and said the harness
could not distinguish them. It can now. The first was **wrong**, and wrong in a
way worth recording:

> *Fixed upstream. The harness pins `hashicorp/raft` v1.7.3. If the Antithesis
> findings were reported and fixed, correct behaviour is exactly what we should
> see.*

There is no release in which they could have been fixed. **v1.7.3 (2025-03-18)
is still the newest tag**, and `main` is 48 commits ahead of it.

Worse, the study did not use v1.7.3 at all. Their harness
([`antithesishq/hashicorp-raft-poc`](https://github.com/antithesishq/hashicorp-raft-poc/tree/antithesis))
is not a *consumer* of raft — its `go.mod` reads `module github.com/hashicorp/raft`.
It is a fork of raft itself with the Antithesis SDK wired in, branched from
`main` at **`4c8f61ac`, 2026-05-19** — 37 commits and fourteen months *newer*
than the tag this harness was pinned to.

So the harness was testing older code than the study, and the report claimed
the opposite direction. Both halves of that were checkable in about ten minutes
and neither was checked.

The harness is now pinned to `4c8f61ac`. It builds unchanged against it, and:

```
full suite                        →   5 passed, 0 failed
test_transfer_during_partition    →  25 passed, 0 failed     (--runs 25)
test_snapshot_install_livelock    →  15 passed, 0 failed     (--runs 15)
```

45 runs against the exact code the bugs were reported in. Still nothing.

### 4.2 "Not enough search" — confirmed, and sharper than stated

Which leaves the second explanation. Checking it properly turned up something
the first draft asserted without verifying: **the 40 runs were never randomized.**

`--runs N` does vary the seed — [`runtime.go:975`](../../internal/star/runtime.go#L975)
sets `seed = uint64(run)`. But a seed only matters if something consumes it, and
the only consumer is probabilistic packet rules
([`packetgateway_wire.go:89`](../../internal/star/packetgateway_wire.go#L89)).
This spec has no `probability=` on any rule and no `choose()` axis. The seed
reaches nothing.

All 45 runs therefore execute the **same fault schedule**: partition at the same
point in the workload, hold for the same duration, release. They differ only in
OS scheduling jitter. Describing that as "40 randomized runs" — as the first
draft of this document did — overstates the search by roughly the entire search.

That is the answer. Both target bugs are races: bug 2 needs the transfer
goroutine mid-replication exactly as the leader steps down; bug 3 needs the
follower's log in a particular state when the snapshot lands. Antithesis reached
them by exploring *when* faults land. This harness explores one point in that
space and repeats it. Forty-five samples of one point is one sample.

**This does not exonerate `hashicorp/raft`, and it is not evidence the bugs are
absent.** It is evidence the experiment was not run.

## 5. Where each Antithesis bug now stands

| Bug | Blocker in the gap analysis | Status |
|---|---|---|
| 1 — async heartbeat race | Needs per-message hold/release attributed to a goroutine | **Still out of scope.** Packet timing influences the window; it does not control it. Needs [RFC-053](https://github.com/faultbox/Faultbox/issues/143) Path D — seccomp reports M, not G. |
| 2 — leadership-transfer deadlock | Gaps 1 and 3: partition on established connections | **Expressible, not yet searched.** `partition_start` + `/transfer` + `partition_stop` runs as intended. 25 runs of one fault schedule; the timing space is unexplored. |
| 3 — snapshot-install livelock | Gaps 1 and 3: sustained isolation of a live peer | **Expressible, not yet searched.** Follower falls 200 commands behind and needs `InstallSnapshot`. 15 runs of one fault schedule. |

The gap analysis's verdict was *"Faultbox can host the workload but cannot yet
break the cluster."* It can break the cluster now — the isolation test proves
it. What it has not yet done is *search*, and §4.2 is the reason. Expressing a
fault and exploring when it lands are different capabilities, and this epic
delivered only the first.

## 6. What is still missing

- **Fault-timing exploration.** `choose()` over partition onset and duration,
  driven by `--explore`, so the search covers the space rather than one point
  in it. §4.2 promotes this from "most likely route to bugs 2 and 3" to **the
  precondition for the question being open at all**. Until it lands, "not
  reproduced" here carries no information about `hashicorp/raft`.
- **A `--runs` that says what it does.** `--runs N` on a spec with no
  seed-consuming construct repeats one schedule N times. That is a reasonable
  flake check and a useless search, and nothing in the output distinguishes
  them. At minimum it should say so.
- **Pre-starting all proxies.** Packet faults reach every mesh link; L7 faults
  (`error()`, `response()`) still do not for links whose peer started later.
  Deferred to v0.14.1 with the caveat documented.
- **RFC-053 Path D** for bug 1. Unchanged.

§4's open question — *which version?* — is closed: `main @ 4c8f61ac`, the
study's own base, 45 runs, no reproduction.

## 7. Reproducing

```bash
cd poc/raft-cluster && GOOS=linux GOARCH=arm64 go build -o /tmp/raft-node .
make env-start
limactl shell faultbox-dev
sudo faultbox test poc/raft-cluster/faultbox.star
```

```
--- PASS: test_baseline_replication        (736ms)
--- PASS: test_isolate_leader_from_quorum  (819ms)   isolate: applies with no quorum = 0
--- PASS: test_one_way_partition           (668ms)
--- PASS: test_snapshot_install_livelock  (2534ms)   converged after round 0
--- PASS: test_transfer_during_partition  (2761ms)   accepted after recovery = 10
5 passed, 0 failed
```

Those timings are from `main @ 4c8f61ac`. Re-running against the v1.7.3 tag —
the original pin — gives the same verdict:

```bash
go get github.com/hashicorp/raft@v1.7.3 && go build -o /tmp/raft-node .
```
