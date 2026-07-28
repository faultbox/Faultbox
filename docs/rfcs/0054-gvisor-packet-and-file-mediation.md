# RFC-054: gVisor Adoption — Packet-Level Network Faults & File-Level I/O Observation

> **Status: Draft**, 2026-07-28. Target: **v0.14.0**. Branch: `epic/v0.14.0-gvisor`.
> Implements [RFC-046](0046-beyond-l1-roadmap.md) **Path B** and introduces a third path
> (**Path C-lite**) that RFC-046 did not consider. Uses the L0–L5 vocabulary from
> [RFC-040](0040-determinism-levels.md). Implementation plan:
> [`docs/implementation/v0.14.0-rfc-054-plan.md`](../implementation/v0.14.0-rfc-054-plan.md).
>
> **Sequencing note:** this work retargets to v0.14.0 and slides
> [RFC-052 (Agent-First Surface)](https://github.com/faultbox/Faultbox/issues/136) to v0.15.0.

## Summary

Faultbox mediates at two layers — individual **syscalls** (seccomp-notify) and parsed
**L7 protocol messages** (the 14 proxies). Between them sits a gap: **a packet is not an
object anywhere in the codebase.** Below the proxies, `write()` is an opaque call with a
length; above the syscalls, an HTTP request is a parsed struct. Neither layer can express
"drop this TCP segment", "delay every ACK from the server", or "corrupt byte 900 of this
datagram". Symmetrically on the filesystem: Faultbox recovers paths out-of-band from
`/proc`, races the SUT while doing it, and matches them with a single-segment glob.

This RFC adopts two **stock, unforked** pieces of gVisor to close both gaps:

1. **`gvisor.dev/gvisor/pkg/tcpip` (netstack) as a Go library** — a userspace TCP/IP stack
   imported like any other dependency. Faultbox wraps its `stack.LinkEndpoint` and gains
   every IP packet crossing the mediated link as a `*stack.PacketBuffer` it can drop,
   delay, reorder, duplicate, corrupt, or answer with a synthetic RST. This is RFC-046's
   Path B, and it makes packet faults a first-class DSL concept.

2. **`runsc` + the Sentry's seccheck trace points** — stock gVisor as an OCI runtime, with
   Faultbox implementing the documented remote-sink protocol over `SOCK_SEQPACKET` UDS.
   The Sentry reports the **resolved** path, byte count, and file offset for every
   `open`/`read`/`write`/`close`/`connect`, with no `/proc` race and no 256-byte truncation.
   RFC-046 did not consider this path; it delivers most of L2's *observation* value without
   the 3–6 month Sentry fork.

It **explicitly does not** fork the Sentry (RFC-046 Path C). Clock virtualization, the RNG
funnel, and true L2/L3 determinism stay out of scope, and the determinism ceiling stays at
**L1**. What changes is the *width* of the mediated surface, not the level.

The DSL grows a `packet_*` fault family with a fast declarative matcher plus a
`where=lambda pkt: ...` escape hatch, and a `watch()` primitive for file-scoped I/O
observation.

## Motivation

### The two gaps, precisely

Every restriction below is a real, current limit with a code reference — not a
hypothetical.

#### Gap 1 — no L3/L4 datapath

| Cannot express today | Root cause |
|---|---|
| Drop, delay, reorder, or duplicate an individual TCP segment | No packet object exists |
| Inject RST mid-stream; shrink the receive window; force retransmits | Same |
| MTU black holes, fragmentation, PMTUD failure | Same |
| Asymmetric latency (fast one way, slow the other), jitter, bandwidth caps | Same |
| Percentage packet loss | `drop` at [`internal/proxy/tcp.go:20`](../../internal/proxy/tcp.go) closes the **whole connection** |
| Corrupt or reorder UDP datagrams | [`internal/proxy/udp.go:14-18`](../../internal/proxy/udp.go) documents both as unimplemented |
| Match on payload bytes | Matching is glob-only over parsed protocol fields; the `tcp` proxy's sole predicate is a byte-prefix on the *first* client chunk ([`tcp.go:27`](../../internal/proxy/tcp.go)) |
| Partition an **established** connection, or partition one direction only | `partition()` is `connect()` deny-by-destination — it cannot touch a socket that already connected |

The last two rows matter more than they look. A `drop` that closes the connection sends a
`RST`, and well-written clients handle `ECONNRESET` correctly on the first try. The bug
class that actually takes production down — a connection stuck in `ESTABLISHED` writing
into a void until a keepalive fires minutes later — is precisely the one Faultbox cannot
currently produce.

#### Gap 2 — filesystem mediation is best-effort and coarse

- Paths are recovered out-of-band: `ReadStringFromProcess(pid, arg, 256)` for path-argument
  syscalls ([`launch_linux.go:317`](../../internal/engine/launch_linux.go)) and
  `readlink /proc/PID/fd/N` for fd syscalls ([`launch_linux.go:323`](../../internal/engine/launch_linux.go)).
  Both race the SUT between notification and response, cap at 256 bytes, and **fail
  silently** — a rule carrying a `path=` glob simply does not match, with no diagnostic.
- `MatchPath` is `filepath.Match` ([`fault.go:18-29`](../../internal/engine/fault.go)) —
  a single path segment, no `**`. `/data/*` does not match `/data/a/b`. The comment in
  the source says *"For the PoC this is fine."*
- The fault vocabulary is per-syscall deny/delay. There is **no byte-level semantics**:
  no short write, no torn write, no partial read, no bit-rot corruption, no
  `ENOSPC`-after-N-bytes, and no *fsync lies then the process dies* — the single most
  valuable durability fault there is.
- `mmap`'d I/O and `io_uring` are invisible. No syscall fires per page fault or per SQE.
- `fs-unmediated` is a **reserved category that emits zero events**
  ([`determinism.go:44-46`](../../internal/star/determinism.go)). Faultbox cannot currently
  tell a user that unmediated filesystem I/O even happened.
- Mediation is opt-in by side effect: seccomp filters install only for services some
  `fault()` targets. Pure observation requires injecting a no-op fault — documented as a
  workaround in [`docs/spec-language.md`](../spec-language.md).

### Why now

- **Customer asks are packet-shaped.** "Delay specific TCP/UDP packets, selected by a
  predicate" and "track I/O under one specific file" are both direct requests. Neither is
  reachable by extending the existing layers — they need a new datapath, not a new kwarg.
- **RFC-050 (gray & metastable faults)** depends on partial, directional, probabilistic
  degradation. Gray failure is *by definition* not a clean connection close. Without a
  packet layer, RFC-050's most interesting scenarios are inexpressible.
- **RFC-040's `fs-unmediated` promise is outstanding.** It ships as reserved syntax with a
  commitment to implement detection later. Path C-lite is how that debt gets paid.
- **The reserved syntax is already in the tree.** `determinism(runtime="gvisor")` parses and
  errors with a pointer to RFC-046 ([`determinism.go:118`](../../internal/star/determinism.go)),
  and `clock="virtual"` errors with *"requires gVisor (Path C)"*
  ([`await.go:240`](../../internal/star/await.go)). The migration was designed to be
  non-breaking; this RFC is the release that uses it.

### If we don't

Faultbox stays a syscall-and-L7 tool. Every network fault remains "the whole connection
died" or "this parsed message was rewritten", which covers retry logic and error handling
but not timeout tuning, backpressure, head-of-line blocking, connection-pool poisoning, or
any metastable failure mode. On the filesystem side, `path=` targeting stays a documented
footgun that silently no-ops when a path exceeds 256 bytes or has more than one directory
level below the glob.

## Verified ground truth

Claims below were checked against `gvisor.dev/gvisor@v0.0.0-20260728023034-41cfc418a32b`
(resolved 2026-07-28), not from memory.

> **Version caveat, from the M0.1 spike:** that pseudo-version is the correct source for the
> API and schema facts below, but it **does not build as a module dependency** — see
> [Decision record M0.1](#decision-record-m01--dependency-viability). The version this RFC
> proposes to depend on is `v0.0.0-20260224225140-573d5e7127a8`. The API surface cited here
> is unchanged between the two.

### Netstack is importable standalone

`gvisor.dev/gvisor/pkg/tcpip` resolves as a normal Go module via pseudo-version. It is
Apache-2.0 and is consumed standalone in production by other Go projects (Tailscale's
`tsnet` is the best-known case), so "netstack without runsc" is a supported configuration,
not an experiment.

The interception point is `stack.LinkEndpoint`, defined at
`pkg/tcpip/stack/registration.go:1266`:

```go
type LinkEndpoint interface {
    NetworkLinkEndpoint   // Attach, MTU, Capabilities, AddHeader, ParseHeader, Close, ...
    LinkWriter            // WritePackets(PacketBufferList) (int, tcpip.Error)
}
```

Inbound packets arrive through `stack.NetworkDispatcher` (`registration.go:1136`).
**`pkg/tcpip/link/sniffer/sniffer.go` is the canonical wrapper reference** — it implements
exactly the decorator this RFC needs (wrap an endpoint, observe every packet in both
directions, delegate). Faultbox's `FaultEndpoint` is a sniffer that is also allowed to say
no.

`pkg/tcpip/link/channel` provides an in-memory endpoint with **no OS dependency**, which
means the entire packet-fault rule engine is unit-testable on a macOS dev host with no
Lima VM, no TUN device, and no privileges. This materially de-risks the "confident nothing
is broken after every milestone" requirement.

### The seccheck remote sink is observe-only — confirmed

From `pkg/sentry/seccheck/sinks/remote/README.md`:

> Upon a new connection, there is a handshake message [...] **This is the only time that
> the monitoring process writes to the socket. From this point on, it only reads a stream
> of trace points generated from the Sentry.**

There is **no enforcement return path**. Path C-lite buys observation, never injection.
This RFC states that limit up front rather than discovering it in month two.

Transport is `SOCK_SEQPACKET` UDS with a fixed header plus protobuf payloads
(`pkg/sentry/seccheck/sinks/remote/wire`). A reference Go server ships in-tree at
`pkg/sentry/seccheck/sinks/remote/server/server.go`, and `tools/tracereplay` records and
replays trace sessions from a file — so Faultbox's decoder can be **tested against
recorded fixtures without runsc**, which again keeps CI honest on non-Linux hosts.

### The trace point schema carries exactly what we need

From `pkg/sentry/seccheck/points/syscall.proto`:

```protobuf
message Write {
  gvisor.common.ContextData context_data = 1;
  Exit   exit       = 2;   // result + errorno
  uint64 sysno      = 3;
  int64  fd         = 4;
  string fd_path    = 5;   // resolved by the Sentry — no /proc race
  uint64 count      = 6;
  bool   has_offset = 7;
  int64  offset     = 8;   // byte offset
  uint32 flags      = 9;
}
```

`Read` is identical. `Open` carries both `fd_path` and `pathname`. `Connect` carries the
raw sockaddr as `bytes address`. Every point carries an `Exit` with `result` and `errorno`.

Two consequences, both worth stating plainly:

- **Better than promised.** Faultbox gets resolved path *plus* byte offset *plus* count
  *plus* the actual errno, per operation. Today it gets a possibly-empty racy path string.
- **No payload bytes.** `Read`/`Write` carry `count`, not content. Content-level FS
  assertions are out of scope for v0.14.0.

## Non-goals

Explicitly out of scope, to keep the release finishable:

- **Forking the Sentry (RFC-046 Path C).** No clock virtualization, no RNG funnel.
  `clock="virtual"` keeps erroring.
- **Raising the determinism ceiling above L1.** `determinism(level="L2")` continues to
  error at spec load. The mediated *surface* widens; the *promise* does not. Claiming L2
  requires total event determinism including clock and RNG, which neither path delivers.
- **Filesystem fault *injection* at byte granularity.** Short writes, torn writes, fsync
  lies, and bit-rot need either a Sentry fork or a FUSE datapath. Deferred; sketched under
  "Future work" so v0.14.0's design doesn't foreclose it.
- **Replacing seccomp-notify.** Path A stays the default engine. Everything here is opt-in
  behind `determinism(runtime=...)`.

## Proposed design

### Component map

```
                    ┌─────────────────────────────────────────────┐
   spec (.star)     │  internal/star — packet_* builtins, Packet   │
                    │  type, watch(), matcher compiler             │
                    └───────────────┬─────────────────────────────┘
                                    │ compiled rules
              ┌─────────────────────┴──────────────────────┐
              ▼                                            ▼
  ┌────────────────────────────┐          ┌──────────────────────────────┐
  │ internal/netfault          │          │ internal/gvisor/seccheck     │
  │  • FaultEndpoint           │          │  • UDS SOCK_SEQPACKET server │
  │    (stack.LinkEndpoint     │          │  • wire header + protobuf    │
  │     decorator)             │          │    decode                    │
  │  • rule engine + matcher   │          │  • point → file_io event     │
  │  • delay/reorder queues    │          │  • path matcher (doublestar) │
  └──────────┬─────────────────┘          └──────────────┬───────────────┘
             │ gvisor.dev/gvisor/pkg/tcpip               │ runsc (stock)
             ▼                                           ▼
   ┌───────────────────────┐                  ┌────────────────────────┐
   │ netstack Stack + NIC  │                  │ Sentry seccheck points │
   │ link/fdbased | channel│                  │ (observe-only)         │
   └───────────────────────┘                  └────────────────────────┘
             │                                           │
             └──────────── internal/star/events.go ──────┘
                        packet / file_io event families
```

### The packet gateway

A new package `internal/netfault` owns a netstack `Stack` per mediated link. The single
interception point is a decorator:

```go
// FaultEndpoint wraps a stack.LinkEndpoint and applies packet fault rules to
// traffic in both directions. Modeled on pkg/tcpip/link/sniffer, which
// implements the same decorator shape for observation only.
type FaultEndpoint struct {
    stack.LinkEndpoint                 // embedded: delegate everything unhandled
    disp   stack.NetworkDispatcher     // upstream, for inbound delivery
    rules  atomic.Pointer[ruleSet]     // installed/removed by fault()/fault_stop()
    sched  *deferQueue                 // delay + reorder without blocking the datapath
    emit   func(PacketEvent)
}

// egress: netstack → wire
func (e *FaultEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error)

// ingress: wire → netstack
func (e *FaultEndpoint) DeliverNetworkPacket(proto tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer)
```

Both directions run the same pipeline: parse headers (`header.IPv4` / `header.TCP` /
`header.UDP`), build a lightweight `PacketView`, evaluate rules in declaration order,
apply the first matching action, emit an event.

**Actions.** `drop` (free the buffer, no RST — the packet simply never existed),
`delay` (hand to `deferQueue`, re-inject after d), `reorder` (hold packet N, release after
N+k), `duplicate` (deliver twice), `corrupt` (flip bytes at an offset, optionally fixing or
deliberately not fixing the checksum), `reset` (synthesize a RST and drop the original),
`window` (rewrite the advertised receive window — including to zero, for backpressure
tests), `pass` (explicit allow, for ordering rules ahead of a broad matcher).

**Ordering and determinism.** `deferQueue` is a single-goroutine timer wheel keyed on a
monotonic tick, not one goroutine per delayed packet. Release order for equal deadlines is
by insertion sequence. This keeps packet reordering reproducible for a given seed, which
matters because the existing per-notification-goroutine model in
[`launch_linux.go:295`](../../internal/engine/launch_linux.go) is exactly the thing L1
refuses to promise ordering for. Packet faults should be *better* than that, not equally
vague.

### Insertion — deliberately unresolved

**This is the RFC's primary open question and is resolved by a Milestone 0 spike, not by
this document.** Getting the SUT's traffic into netstack is the highest-risk unknown, and
committing to a mechanism before prototyping it would be guessing. Three candidates:

| Candidate | How | Pros | Cons |
|---|---|---|---|
| **A. TUN + routed subnet** | Faultbox owns a TUN device and a dedicated subnet (`10.99.0.0/16`); the existing RFC-024 env rewrite points the SUT at a netstack address | Reuses the shipped insertion contract unchanged; works for binary **and** container mode; no netns surgery | Needs `CAP_NET_ADMIN` + `ip_forward`; container→host routing needs care |
| **B. veth + AF_PACKET** | Docker creates the netns; Faultbox attaches a raw socket to the host-side veth peer and feeds L2 frames to netstack | Captures **all** container traffic, including hardcoded addresses — closes the RFC-024 bypass that is also L1's `network-unmediated` blind spot | Container mode only; more sensitive to Docker version and network driver |
| **C. netns + fdbased** | Faultbox pre-creates a netns with a TAP device, container joins via `NetworkMode: container:<sidecar>` | Cleanest isolation | Most Docker plumbing; interacts badly with the existing container network setup |

Milestone 0 builds a throwaway prototype of A and B against a real Postgres container and
picks on evidence: does traffic actually arrive, what is the added latency, does it survive
container restart, does it work in the Lima VM and in CI. The spike's output is a decision
record appended to this RFC.

The rule engine, matcher, DSL, and event schema are **independent of this choice** — they
sit above the link endpoint — so Milestones 1–3 proceed in parallel against a
`link/channel` endpoint regardless of how the spike lands.

### File-level I/O observation

`internal/gvisor/seccheck` implements the documented remote-sink protocol: create the UDS
before the sandbox starts, accept the handshake, then decode a stream of points. Each
relevant point becomes a `file_io` event:

| Field | Source | Notes |
|---|---|---|
| `path` | `fd_path` / `pathname` | Sentry-resolved; no `/proc` race, no 256-byte cap |
| `op` | point type | `open`, `read`, `write`, `close`, `fsync` |
| `fd` | `fd` | |
| `offset` | `offset` (when `has_offset`) | new capability — absent today |
| `count` | `count` | bytes requested |
| `result` | `exit.result` | bytes actually transferred |
| `errno` | `exit.errorno` | real errno, not inferred |
| `pid`, `tid` | `context_data` | |

Path matching moves to a **doublestar** matcher (`**` across segments) shared by the new
`watch()` primitive *and* retrofitted onto the existing syscall `PathGlob` — so
`op(path="/data/**/*.wal")` starts working on Path A too. That retrofit is a genuine bug
fix that ships regardless of whether a user adopts gVisor.

Enabling this requires the container to run under `runsc`, which is a Docker daemon-level
runtime registration. Faultbox detects its availability and fails at spec load with an
actionable message rather than at run time.

### Runtime selection — a proposed refinement to RFC-046

RFC-046's capability matrix has a single `runtime="gvisor"` value. That conflates two
mechanisms with very different adoption costs: the packet gateway needs no runsc at all,
while FS observation requires every container in the spec to run under runsc. Gating the
cheap half behind the expensive half would be a mistake.

Proposed values (spec-wide, per RFC-040's rule that the engine is one choice for the whole
spec):

| `runtime=` | Packet gateway | FS observation | Requires | Caps at |
|---|---|---|---|---|
| `"default"` | — | — | — | L1 |
| `"gvisor-net"` | ✅ | — | `CAP_NET_ADMIN` | L1 |
| `"gvisor"` | ✅ | ✅ | runsc + container mode for every service | L1 |

Consistent with RFC-046's honesty rule: a spec that can't run every service under the
chosen engine lowers the runtime rather than silently mixing engines. **This deviates from
RFC-046's binary matrix and is listed as an open question.**

## DSL extensions

### Naming: why `packet_*` and not another overload

`delay()` and `drop()` are already overloaded twice — syscall-level when given a positional
duration, protocol-level otherwise. The docs carry an explicit warning about this, and
v0.13.2/0.13.3 shipped a round of fixes for kwargs being silently swallowed by the wrong
overload. A **third** context-dependent meaning would be actively harmful. Packet faults
therefore get their own unambiguous prefix.

### Packet fault builtins

Targeted at an interface reference, matching the existing `fault(db.main, ...)` shape:

```python
packet_drop(**match)
packet_delay(duration, **match)
packet_reorder(by=2, **match)                 # release this packet after `by` later ones
packet_duplicate(count=2, **match)
packet_corrupt(offset=0, length=1, mode="flip"|"zero"|"random", checksum="fix"|"break", **match)
packet_reset(**match)                         # synthesize RST, drop the original
packet_window(size=0, **match)                # rewrite advertised receive window
```

Plus two link-scoped shapers that take no matcher:

```python
bandwidth(rate="1mbps", dir="c2s")
mtu(size=576)
```

### The matcher — declarative fast path, lambda escape hatch

Every `packet_*` builtin accepts the same match kwargs, all optional and ANDed:

| Kwarg | Type | Meaning |
|---|---|---|
| `dir` | `"c2s"` / `"s2c"` / `"both"` | direction, default `"both"` |
| `proto` | `"tcp"` / `"udp"` / `"icmp"` | |
| `flags` | string | TCP flags, e.g. `"SYN"`, `"PSH,ACK"`, `"!RST"` |
| `port` | int | destination port |
| `len_gt`, `len_lt`, `len` | int | payload length |
| `payload_prefix` | string | byte prefix |
| `payload_contains` | string | substring |
| `nth`, `after`, `every` | int | occurrence selectors, mirroring the existing `TriggerNth` / `TriggerAfter` |
| `probability`, `max_fires`, `mode` | — | **reused verbatim** from the syscall fault semantics, including RFC-042 §8.9 exhaustive fan-out |
| `where` | callable | escape hatch — see below |

The declarative kwargs compile to a closed-form predicate evaluated in Go with zero
allocations and no Starlark on the datapath. `where=` accepts a Starlark lambda receiving a
`Packet` value:

```python
fault(db.main,
    packet_delay("250ms", where = lambda p: p.dir == "c2s" and p.len > 1400),
    run = scenario,
)
```

**Cost and honesty about it.** A Starlark call per packet is roughly a microsecond and
takes a lock. The runtime therefore (a) evaluates declarative kwargs *first* and only calls
the lambda for packets that already passed them, so `where=` composes as a refinement
rather than a replacement; (b) emits a warning to the report when a `where=` lambda is
evaluated more than a configurable threshold within one test; (c) documents that lambdas
must be pure — a lambda reading external state breaks the L1 replay contract, and Faultbox
cannot detect that.

### The `Packet` type

Read-only, constructed per evaluation, fields backed by the parsed headers:

| Field | Type | |
|---|---|---|
| `proto` | string | `"tcp"` / `"udp"` / `"icmp"` |
| `dir` | string | `"c2s"` / `"s2c"` |
| `src_ip`, `dst_ip` | string | |
| `src_port`, `dst_port` | int | |
| `len` | int | payload length, headers excluded |
| `flags` | list of string | TCP only |
| `seq`, `ack`, `window` | int | TCP only |
| `payload` | bytes | capped (default 4 KiB, configurable) |
| `index` | int | per-flow ordinal, 0-based |
| `flow` | string | stable flow identifier, usable as a dict key |

### File-scoped observation

```python
watch(pg,
    files = ["/var/lib/postgresql/**/pg_wal/*"],
    ops   = ["write", "fsync", "open"],
    run   = scenario,
)
```

Emits `file_io` events. Scoped like `fault()` — `watch_start()` / `watch_stop()` provided
for imperative use, mirroring `trace_start` / `trace_stop`.

Query helper for assertions:

```python
io = file_io(service = pg, path = "**/pg_wal/*")
assert_true(io.bytes_written > 0)
assert_true(io.count(op = "fsync") >= 1)
```

And the existing temporal machinery works unchanged, since `file_io` is an ordinary event
family:

```python
assert_ordered(
    lambda e: e.type == "file_io" and e.data["op"] == "fsync",
    lambda e: e.type == "http" and e.data["status"] == "200",
)
```

### Proposed scenarios

The user asked for scenario proposals. These are the ones that are **impossible today** and
become natural — ordered by how often they correspond to a real outage.

1. **Silent blackhole (half-open connection).** Drop `dir="s2c"` packets *after* the
   handshake completes. The client stays in `ESTABLISHED` writing into a void until its
   keepalive or application timeout fires. This is the canonical "service hung, no error"
   incident and, as noted above, is the single most important thing today's `drop` cannot
   reproduce — because closing the connection sends a RST that clients handle correctly.

2. **Gray partition.** `packet_drop(dir="c2s", probability="30%")` — partial, directional,
   probabilistic. The direct enabler for [RFC-050](0050-gray-metastable-faults.md)'s
   metastable-failure scenarios, which cannot be built from clean connection closes.

3. **Connection-pool poisoning.** `packet_reset(after=100)` — RST mid-stream after 100
   packets. Tests whether a pool detects and evicts a dead connection or keeps handing it
   to callers.

4. **Zero-window stall / backpressure.** `packet_window(size=0, dir="s2c")` — the server
   advertises a full receive buffer. Tests whether the SUT applies backpressure or buffers
   unboundedly until OOM.

5. **Asymmetric latency.** `packet_delay("400ms", dir="s2c")` with `dir="c2s"` untouched.
   Breaks every naive RTT-based timeout heuristic and every "measure once at startup"
   adaptive timeout.

6. **Retransmit storm.** `packet_drop(dir="s2c", every=3, flags="PSH,ACK")` — drop every
   third data segment, forcing continuous retransmission. Exposes head-of-line blocking and
   throughput collapse under loss.

7. **UDP reorder & duplicate.** `packet_reorder(by=2, proto="udp")` and
   `packet_duplicate(proto="udp")` — metrics double-counting, gossip-protocol convergence,
   and DNS response-matching bugs. Explicitly listed as unimplemented in `udp.go` today.

8. **MTU black hole.** `mtu(576)` plus dropping ICMP fragmentation-needed. The classic
   VPN/overlay-network failure that works fine for small requests and breaks the moment a
   payload crosses the threshold.

9. **Payload-triggered fault.** `packet_delay("2s", where=lambda p: p.payload.startswith(b"\\x00\\x00\\x00"))`
   — target a specific message type inside a custom binary protocol Faultbox has no plugin
   for. This is the InfoSec-persona case from RFC-046 that motivated Path B.

10. **WAL durability audit.** `watch(pg, files=["**/pg_wal/*"], ops=["write","fsync"])` plus
    `assert_ordered(fsync_event, http_200_event)` — prove the database fsynced the WAL
    *before* the commit was acknowledged. Provable only with exact-path and per-operation
    ordering.

11. **I/O surface audit.** `assert_never(lambda e: e.type == "file_io" and not
    e.data["path"].startswith("/var/lib/app"))` — the SUT never touches anything outside
    its data directory. A compliance and InfoSec use case, and a concrete down payment on
    RFC-046's L4 Hermetic mode.

12. **Write-amplification / torn-record detection.** With `offset` and `count` per write,
    assert that a record update lands as a single write rather than several — the
    precondition for crash-atomicity that most storage code assumes and few verify.

## Determinism impact

Nothing here raises the ceiling above L1, and the RFC deliberately does not claim
otherwise. What changes:

- **`network-unmediated` gets much stronger** under insertion candidate B, which sees all
  container traffic rather than only what env rewriting redirected.
- **`fs-unmediated` becomes real** for the first time. Under `runtime="gvisor"`, Faultbox
  can finally emit the events RFC-040 reserved the category for.
- **Packet ordering is reproducible** for a given seed within the gateway, because the
  defer queue is a single-goroutine timer wheel with insertion-order tiebreaks.
- **`where=` lambdas are a new nondeterminism risk.** An impure lambda silently breaks
  replay. Documented; not detectable.
- **Netstack's own timers are wall-clock.** TCP retransmit and keepalive timers inside
  netstack are not virtualized, so a test that depends on a retransmit deadline is
  wall-clock sensitive — the same honesty caveat L1 already carries.

`determinism(level="L2")` continues to error. L2 means *total* event determinism including
clock and RNG; a wider observation surface is not the same thing, and conflating them would
repeat exactly the overclaiming RFC-040 was written to stop.

## Impact

- **Breaking changes:** none. Everything is behind `determinism(runtime=...)`, which
  currently errors for any non-default value, so no existing spec can be affected. The
  doublestar path-matcher retrofit is a widening — patterns that match today continue to
  match.
- **Migration:** none required.
- **Dependencies:** `gvisor.dev/gvisor` is a large module. Expected binary growth of
  several MB and a longer cold build. Measured in Milestone 1 and reported; if the number
  is unacceptable the fallback is a build tag that compiles the gateway out.
- **Platform:** the gateway is Linux-only at runtime (`link/fdbased`), but the rule engine
  and matcher build and test everywhere via `link/channel`. macOS developers keep a working
  `make test`.
- **Performance:** pass-through adds one endpoint hop per packet. Benchmarked in
  Milestone 1 against the existing proxy numbers from RFC-024.
- **Security:** the gateway binds no new externally-reachable ports. The seccheck UDS is
  created mode `0600` in the session directory. Per gVisor's own threat model the Sentry is
  untrusted, so the decoder **must** enforce hard size limits on every field — called out
  as a test requirement, not a code comment.

## Alternatives considered

- **Extend the existing proxies with pseudo-packet faults.** Chunk the `io.Copy` stream and
  delay chunks. Cheap, and genuinely wrong: a chunk is not a segment, there are no flags, no
  sequence numbers, no window, and no way to produce a retransmit. It would ship a feature
  named "packet fault" that cannot express any of the twelve scenarios above.
- **`tc netem` / `iptables` in the container netns.** Real packet manipulation with no new
  dependency. Rejected: no payload-aware predicates, no per-flow occurrence counters, no
  integration with the event log or the plan tree, and it requires privileged containers.
  It is a fine *operator* tool and a poor *test-framework* primitive.
- **eBPF.** Powerful and precise. Rejected: CLAUDE.md commits to seccomp-notify explicitly
  *without* eBPF; it needs kernel-version-specific bytecode, complicates the Lima and CI
  story, and cannot run the Starlark predicate escape hatch.
- **Fork the Sentry now (RFC-046 Path C).** Solves everything including clock and RNG.
  Rejected for v0.14.0 on cost: 3–6 months for a release whose two headline asks are
  reachable in weeks. Kept as the L2/L3 path.
- **FUSE for filesystem faults instead of seccheck.** Would deliver byte-level *injection*
  — the one thing Path C-lite cannot do. Not rejected, deferred: it is a separate datapath
  with its own lifecycle, and observation is the stated ask. See Future work.

## Open questions

1. **Insertion mechanism** — TUN+routed subnet, veth+AF_PACKET, or netns+fdbased. Resolved
   by the Milestone 0 spike. *Blocking for Milestone 4, not for 1–3.*
2. **`gvisor-net` as a distinct runtime value** — accept the three-value matrix proposed
   here, or keep RFC-046's binary `gvisor` and require runsc for packet faults too? This
   RFC argues for the split; RFC-046's author should weigh in.
3. **Payload cap for `Packet.payload`** — 4 KiB default proposed. Too small for
   payload-matching on large messages, too large to copy per packet at line rate. Should it
   be per-rule rather than global?
4. **`where=` lambda budget** — warn only, or fail the test past a threshold? Warning risks
   a user shipping a spec that is 100× slower than they think.
5. **Does `runsc` availability gate spec load or service start?** Spec load gives a better
   error; service start allows a spec to be valid on a machine without runsc and merely
   unrunnable.
6. **Do packet faults participate in `fault_matrix()` generation?** They have a much larger
   parameter space than syscall faults, and naive enumeration would explode the plan tree.
7. **Interaction with TLS (RFC-038).** Packet faults operate below TLS, so a corrupt on an
   encrypted stream produces a MAC failure rather than a semantic corruption. Is that the
   intended semantics, or should `packet_corrupt` refuse to target a TLS interface?

## Future work

- **FUSE datapath for byte-level filesystem injection** — short writes, torn writes, fsync
  lies, bit-rot, `ENOSPC` after N bytes. The natural v0.15.x follow-on, and the reason
  `watch()` is specified as observation-only rather than as a fault primitive: the fault
  vocabulary should be designed once, against the datapath that can actually implement it.
- **QUIC / HTTP-3**, which becomes tractable once packets are first-class.
- **Path C (Sentry fork)** for L2/L3, unchanged from RFC-046.
- **pcap export.** `link/sniffer` already writes pcap; exposing `--pcap out.pcap` would let
  users open a failing Faultbox run in Wireshark. Small, and likely to be popular.

## Implementation plan

Six milestones. Each ends with a green `make lint && make test` on macOS **and** the Linux
CI matrix, so a regression is attributable to one milestone. Detail, task breakdown, and
per-milestone test lists live in
[`docs/implementation/v0.14.0-rfc-054-plan.md`](../implementation/v0.14.0-rfc-054-plan.md).

| M | Title | Exit criterion |
|---|---|---|
| **0** | Spikes & decisions | Insertion mechanism chosen on evidence; runsc verified in Lima; dependency cost measured |
| **1** | Netstack foundation | `FaultEndpoint` + rule engine + defer queue, fully tested over `link/channel`; benchmarks published |
| **2** | Packet DSL | `packet_*` builtins, matcher compiler, `Packet` type, `packet` event family; spec-load validation |
| **3** | Path matcher retrofit | doublestar matching shipped for existing `op(path=)` / `PathGlob` — **user-visible fix independent of gVisor** |
| **4** | Gateway insertion | End-to-end packet faults against a real container in Lima; all twelve scenarios as executable specs |
| **5** | FS observation | seccheck sink, `watch()`, `file_io` events, `fs-unmediated` detection |
| **6** | Release | Docs, CHANGELOG, tutorial chapter, RFC status → Implemented, v0.14.0 |

Milestone 3 is deliberately placed early and is independent of every gVisor decision: it
fixes a real bug (`/data/**` silently not matching) that users hit today, so the release
delivers value even if a later milestone slips.

## Decision records

Milestone 0 produces evidence-backed answers to the open questions. Recorded here as they
land.

### Decision record M0.1 — Dependency viability

**Date:** 2026-07-28. **Status:** resolved. **Outcome:** proceed, with a pinned version.

**Pin `gvisor.dev/gvisor v0.0.0-20260224225140-573d5e7127a8` (2026-02-24). Do not track
master.**

The head pseudo-version `v0.0.0-20260728023034-41cfc418a32b` **cannot be built as a Go
module dependency at all**:

```
$ go build gvisor.dev/gvisor/pkg/tcpip/stack
found packages stack (addressable_endpoint_state.go) and bridge (bridge_test.go)
  in .../gvisor.dev/gvisor@v0.0.0-20260728023034-41cfc418a32b/pkg/tcpip/stack
```

`pkg/tcpip/stack` contains two *different* external test package names in one directory —
35 files `package stack`, 6 files `package stack_test`, and one file `package bridge_test`.
That is invalid Go packaging, and it fails at import resolution, so no consumer of
`pkg/tcpip/stack` can build. gVisor is built with Bazel upstream, which does not enforce
the go-tool rule, so this is invisible to the gVisor CI.

The chosen pin is the version [Tailscale](https://github.com/tailscale/tailscale/blob/main/go.mod)
depends on — a useful canary, since a break there is noticed quickly by a large consumer.

**Consequence for the plan:** dependency updates are a deliberate, tested step, never a
routine `go get -u`. Milestone 1 adds a CI check that the pinned version still builds on
all three targets, so an attempted bump fails loudly.

**Everything else measured green:**

| Check | Result |
|---|---|
| `darwin/arm64` build | ✅ |
| `linux/arm64`, `CGO_ENABLED=0` | ✅ |
| `linux/amd64`, `CGO_ENABLED=0` | ✅ |
| `link/fdbased` (Linux insertion path, M4) | ✅ |
| `link/channel`, `link/sniffer`, `ipv4`, `ipv6`, `tcp`, `udp`, `icmp`, `adapters/gonet` | ✅ all targets |
| **Go version bump required?** | **No.** The pin declares `go 1.25.5`; the project is on `go 1.26.1`. (Head would have forced `go >= 1.26.3` — another reason not to track master.) |
| **`FaultEndpoint` decorator shape compiles and runs?** | **Yes.** A `stack.LinkEndpoint` decorator wrapping `link/channel` was accepted by `stack.CreateNIC`; NIC came up at MTU 1500. The design in §"The packet gateway" is validated, not assumed. |
| Binary cost | Minimal main + the full netstack set = **6.5 MB**. Against `bin/faultbox`'s 47.4 MB baseline that is roughly **+13%** |
| `go mod tidy` cost | ~4 min cold — gVisor's module graph is large. A CI cold-cache consideration, not a build-time one |

**No `nogvisor` build tag is needed.** The standing-risk fallback in the plan is withdrawn:
+13% is not worth a second build configuration and the maintenance it implies.

## Dependencies

- **[RFC-040](0040-determinism-levels.md)** — L0–L5 vocabulary; the `runtime=` kwarg this
  RFC activates; the `fs-unmediated` category this RFC finally implements.
- **[RFC-046](0046-beyond-l1-roadmap.md)** — Path B is implemented here. Path C-lite is new
  and proposed as an amendment. The runtime capability matrix is proposed for revision.
- **[RFC-024](0024-proxy-datapath.md)** — env-rewrite insertion, reused by candidate A.
- **[RFC-050](0050-gray-metastable-faults.md)** — the primary consumer of directional,
  probabilistic packet loss.
- **[RFC-038](0038-tls-aware-proxy.md)** — open question 7.
- **[RFC-042](0042-exploration-plan.md)** — `probability` / `max_fires` / `mode` semantics
  reused verbatim; open question 6.

## References

- `gvisor.dev/gvisor@v0.0.0-20260728023034-41cfc418a32b` — Apache-2.0. Verified 2026-07-28.
- `pkg/tcpip/stack/registration.go:1266` — `LinkEndpoint`; `:1136` — `NetworkDispatcher`.
- `pkg/tcpip/link/sniffer/sniffer.go` — decorator reference implementation.
- `pkg/tcpip/link/channel` — OS-independent endpoint; the basis of host-runnable tests.
- `pkg/sentry/seccheck/sinks/remote/README.md` — protocol; confirms observe-only.
- `pkg/sentry/seccheck/points/syscall.proto` — point schema (`fd_path`, `offset`, `count`, `exit`).
- `tools/tracereplay` — record/replay trace sessions for fixture-based tests.
