# Can Faultbox find the Antithesis Raft bugs?

**Date:** 2026-07-28
**Subject:** [Finding bugs in Raft implementations](https://antithesis.com/blog/2026/finding-bugs-in-raft-implementations/) (Antithesis, 2026) — reference harness at [antithesishq/hashicorp-raft-poc](https://github.com/antithesishq/hashicorp-raft-poc/tree/antithesis)
**Status:** Investigation complete. Answer today: **no.** Three concrete gaps explain why.
**Artifacts:** `poc/raft-cluster/` — working 3-node hashicorp/raft cluster under Faultbox, with the chain-of-blocks workload and a state-machine-safety monitor.

---

## 1. What Antithesis found

Three bugs in `hashicorp/raft`, all reachable — per the article — with **network
partitions and turbulence alone**: "no node kill/restart required, or disk
corruption, or other faults."

| # | Bug | Root cause | Property violated |
|---|-----|-----------|-------------------|
| 1 | Broken consensus via async heartbeats | Heartbeats are handled on the transport thread instead of being queued for the main loop, so `currentTerm` mutates concurrently with `dispatchLogs()` | Log Matching, Leader Completeness, State Machine Safety, Election Safety |
| 2 | Deadlock after leadership transfer | Transfer goroutine blocks on replication that aborts when the leader steps down; the global "transfer in progress" flag never clears | Liveness |
| 3 | Livelock in snapshot installation | Violates Raft rule 7 (`InstallSnapshot` must discard the entire log); the follower then fails the `prevLogIndex` check on the next `AppendEntries`, so the leader re-sends a snapshot forever | Liveness |

Their workload is deliberately trivial — a "chain of blocks" FSM that folds each
applied command into a running hash, so state is `(count, hash)`. Any two nodes
that apply different commands at the same index diverge visibly and permanently.
The article notes this could be "written in an afternoon."

**The workload is not the hard part. The fault model is.**

## 2. What was built

`poc/raft-cluster/` is a faithful port of that harness:

- **`main.go`** — a `hashicorp/raft` v1.7.3 node with a TCP transport and the
  chain-of-blocks FSM. Every `Apply`, every leadership observation, and every
  rejected command is written to stdout as one JSON line.
- **`faultbox.star`** — three peer services, each with a `raft` tcp interface and
  an `admin` http interface. Peers discover each other through the auto-injected
  `FAULTBOX_NODEn_RAFT_ADDR` env vars. A spec-wide `monitor()` enforces state
  machine safety: the first node to report prefix length *n* fixes the expected
  hash; any node that later reports a different hash for the same *n* fails the
  test.

The good news first: **this all works.** Baseline replication passes, the three
nodes converge on the same chain, and the observability half of the Antithesis
harness maps onto Faultbox almost one-for-one — their Python `raft-monitor`
sidecar becomes a ten-line `monitor()` over `observe.stdout(decoder=decoder("json"))`.

```
--- PASS: test_baseline_replication (387ms, seed=8411319662338507628) ---
state: {"count":20,"hash":"01903cdb…","node":"node1","state":"Leader","term":2}
state: {"count":19,"hash":"f8a494aa…","node":"node2","state":"Follower","term":2}
state: {"count":19,"hash":"f8a494aa…","node":"node3","state":"Follower","term":2}
```

## 3. The measured result

Four fault scenarios were run against the formed cluster: `partition(node1,
node2)`, `fault(node2.raft, drop())`, `fault(node3.raft, delay(delay="800ms"))`,
and — the decisive one — `drop()` on **both** followers simultaneously, which
should isolate the leader from its entire quorum.

Every one of them produced results **identical to a no-fault run**:

```
isolate: applies during full isolation = 88     # 30 more commands committed
isolate: {"count":30,"hash":"2b41f3af…","node":"node1","state":"Leader","term":2}
isolate: {"count":29,"hash":"8ba3cbf9…","node":"node2","state":"Follower","term":2}
isolate: {"count":29,"hash":"8ba3cbf9…","node":"node3","state":"Follower","term":2}
isolate: leader_elected=3 leader_lost=0 apply_rejected=0
--- PASS: test_isolate_leader_from_quorum (591ms) ---
```

A leader cut off from both of its followers committed 30 more commands, never
lost leadership, and never advanced its term. **The faults did not reach the
consensus layer at all.**

That is not a Raft bug. It is three Faultbox gaps stacked on top of each other.

### Gap 1 — `partition()` only blocks connection *setup*

[`builtins.go:2281-2299`](../../internal/star/builtins.go#L2281-L2299) implements
`partition()` as a `connect` syscall deny filtered by destination address.
`hashicorp/raft`'s `NetworkTransport` pools long-lived TCP connections; once the
cluster forms, peers stop calling `connect`. The trace makes this unambiguous —
during the entire partition window, **zero `connect` syscalls were intercepted**:

```
test_partition_leader_from_followers
  {'stdout': 259, 'proxy_started': 6, 'partition_applied': 1, 'partition_removed': 1, …}
  # no 'syscall' events at all
```

`partition_applied` fired. The rule matched nothing, because there was nothing
left to match. Two secondary limits compound this: the deny is symmetric (no
one-way partitions, which is where Raft's interesting failures live), and the
destination is hardcoded to `127.0.0.1:<port>`, which cannot match
container-to-container traffic on a Docker network. The container case was
inferred from the code, not measured.

### Gap 2 — proxies are not in the path of a peer mesh

[`runtime.go:2058-2064`](../../internal/star/runtime.go#L2058-L2064):
`preStartProxies` starts an interface's proxy when its owning service starts, and
only services launched *afterwards* get env vars pointing at it. That is correct
for a dependency tree. A peer mesh is a **cycle**, so no start order puts every
link behind a proxy. In the first run, every node advertised its real port:

```
{"event":"node.started","node":"node1","bind":"127.0.0.1:8301","advertise":"localhost:8301"}
```

Faultbox was not on any peer link, so `fault(node2.raft, drop())` was a no-op by
construction.

This one has a cheap fix. The tcp proxy dials its upstream lazily, per connection
(`tcp.go` `handle()`), so a proxy can be started before its upstream exists.
Pre-starting **all** proxies before launching **any** service would remove the
ordering constraint entirely.

### Gap 3 — the tcp proxy only inspects the first chunk

This is the load-bearing gap, and it survives fixing the other two.

The experiment was rerun with startup order reversed (node3 → node2 → node1) and
node1 as sole bootstrapper, which does put node2 and node3 behind proxies:

```
{"event":"raft.bootstrap","node":"node1",
 "peers":"node1=localhost:8301,node2=127.0.0.1:44651,node3=127.0.0.1:46869"}
```

Peer traffic now genuinely flows through Faultbox. The isolation test still had
no effect, because [`tcp.go:132-150`](../../internal/proxy/tcp.go#L132-L150)
peeks the first 4 KiB of a connection, evaluates rules against it **once**, and
then hands both directions to `io.Copy` for the life of the connection. The
event ordering shows it exactly:

```
proxy_conn_open node3        # ← peer link established during cluster formation
proxy_conn_open node2
proxy_fault_applied node2    # ← rules installed after the only rule check ran
proxy_fault_applied node3
```

Every `AppendEntries` and heartbeat after connection setup is invisible to the
rule engine. And *every* Raft message that matters travels on an
already-established connection.

## 4. Why this blocks all three bugs

| Bug | What it needs | Why it is unreachable today |
|---|---|---|
| 1 — async heartbeat race | Deliver a heartbeat carrying a new term *into a specific window* on an established connection, while a `RequestVote` with a different term is in flight | Requires per-message interception mid-stream, plus the ability to hold and release individual messages. Gap 3. |
| 2 — leadership-transfer deadlock | Trigger a transfer, then cause lease timeout / quorum loss on established links before replication completes | The workload half exists (`POST /transfer` is wired up); the fault half needs an established-connection partition. Gaps 1 and 3. |
| 3 — snapshot livelock | Let a follower fall far enough behind to require `InstallSnapshot` | Requires sustained isolation of a live peer. Gaps 1 and 3. |

## 5. What Faultbox already has

Worth stating plainly, because the gap is narrower than "we can't do distributed
systems":

- **Observability is a genuine strength.** `observe.stdout(decoder=decoder("json"))`
  plus `monitor()` replaces their whole Python monitor sidecar. Vector clocks,
  `happens_before`, and the causal trace API go beyond what the reference harness
  checks.
- **Multi-peer topologies run fine.** Three mutually-referencing services boot,
  form a cluster, and replicate — the `FAULTBOX_*` env var injection solves
  Starlark's forward-reference problem cleanly.
- **The search loop exists.** `--runs`, `--seed`, `--explore`, `choose()`,
  `nondet()`, `assume()`, `halt()`, and `.fb` replay bundles are the scaffolding
  for randomized exploration with deterministic replay.
- **Held syscalls are a real scheduling primitive.** `HoldQueue` +
  `ExploreScheduler` already pause a syscall and release it in a chosen order.
  That is the right shape for bug 1 — it is just not currently reachable for
  socket reads/writes on a peer link.

The missing piece is narrow and specific: **fault injection on established
connections, targeted per peer pair.**

## 6. Recommended work

Roughly in dependency order. Items 1–3 together are what "reproduce the
Antithesis study" costs.

1. **Per-chunk rule evaluation in the tcp proxy.** Replace the first-chunk peek +
   `io.Copy` splice with a loop that evaluates rules on every chunk in both
   directions. Unlocks mid-stream drop, delay, and blackhole. *This is the single
   highest-value change.*

2. **Implement `source=`.** It is documented (`spec-language.md:1881-1889`),
   parsed, and emitted into the trace — but
   [`builtins.go:2537-2540`](../../internal/star/builtins.go#L2537-L2540) never
   passes it to the rule, and `proxy.Rule` has no source field. The proxy already
   knows `client.RemoteAddr()`. Without this, a rule on `node2.raft` hits all
   peers at once and asymmetric partitions are inexpressible.

3. **Pre-start all proxies before launching any service.** Removes the mesh
   ordering constraint (gap 2). Small change; the proxy already dials lazily.

4. **Rebuild `partition()` on the proxy path** rather than on `connect` deny, with
   `partition_start`/`partition_stop` and a `direction=` argument for one-way
   partitions. The current implementation should be treated as a known-limited
   primitive until then — as measured, it silently does nothing against any
   service that pools connections, which is most real infrastructure.

5. **Extend `DestAddr` matching beyond `connect`.** Record the fd returned by
   `connect`/`accept`, then let `read`/`write` faults filter by peer. This is the
   syscall-level path to the same capability, and it is what would make bug 1
   reachable via `HoldQueue` — hold a peer `read` until a chosen interleaving
   point, then release.

6. **Service kill/restart builtins.** Not required for these three bugs, but the
   obvious next fault class once partitions work.

### Two defects found along the way

Both were hit while writing the harness, and neither is Raft-specific:

- **`source=` is silently ignored** (item 2 above). A user writing
  `fault(kafka.main, source=worker, drop(...))` — the exact example in the docs —
  gets a rule that fires for every consumer, with no warning.
- **The documented `monitor()` example cannot work.**
  `spec-language.md:1162-1165` shows `event.data.get("level", "")` inside a
  monitor `check=`. Monitors receive an `EventVal` ([`trace.go:241`](../../internal/star/trace.go#L241)),
  which has no `.data` attribute and falls through to a raw string field lookup —
  so the example fails with `string has no .get field or method`. Only
  `StarlarkEvent` (used by `where=` predicates) auto-decodes `.data` into a dict.
  Either give `EventVal` the same auto-decode, or fix the docs to use flat field
  access (`event.level`), which does work.

## 7. Verdict

Faultbox can host the Antithesis Raft workload today — topology, client
workload, structured event ingestion, and cross-node safety monitors all work,
and the POC in `poc/raft-cluster/` demonstrates it end to end.

It cannot yet *break* the cluster. Its two partition mechanisms both act only at
connection-establishment time, and consensus protocols do their interesting work
on connections that are already open. Items 1–3 above close that gap; they are
proxy-layer changes, not architectural ones.
