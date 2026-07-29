# RFC-054: gVisor Adoption — Packet-Level Network Faults & File-Level I/O Observation

> **Status: Implemented.** Packet faults v0.14.0; `bandwidth()`/`mtu()` v0.14.1.
> `watch()` is **split out to [RFC-056](0056-filesystem-observation.md)**, target v0.15.0 —
> the tracing mechanism is settled (see the
> [`-pod-init-config` spike](../design/2026-07-29-pod-init-config-spike.md)); what remains
> is host-configuration lifecycle, which is its own design.
> 2026-07-28. Target: **v0.14.0**. Branch: `epic/v0.14.0-gvisor`.
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

### Insertion — resolved by spike

> **Resolved 2026-07-28: candidate A (TUN + routed subnet).** Full evidence in
> [Decision record M0.2](#decision-record-m02--insertion-mechanism). The table below is the
> pre-spike framing, kept for context.

Getting the SUT's traffic into netstack was the highest-risk unknown, so it was prototyped
rather than argued. Three candidates were considered:

| Candidate | How | Pros | Cons |
|---|---|---|---|
| **A. TUN + routed subnet** | Faultbox owns a TUN device and a dedicated subnet (`10.99.0.0/16`); the existing RFC-024 env rewrite points the SUT at a netstack address | Reuses the shipped insertion contract unchanged; works for binary **and** container mode; no netns surgery | Needs `CAP_NET_ADMIN` + `ip_forward`; container→host routing needs care |
| **B. veth + AF_PACKET** | Docker creates the netns; Faultbox attaches a raw socket to the host-side veth peer and feeds L2 frames to netstack | Captures **all** container traffic, including hardcoded addresses — closes the RFC-024 bypass that is also L1's `network-unmediated` blind spot | Container mode only; more sensitive to Docker version and network driver |
| **C. netns + fdbased** | Faultbox pre-creates a netns with a TAP device, container joins via `NetworkMode: container:<sidecar>` | Cleanest isolation | Most Docker plumbing; interacts badly with the existing container network setup |

The rule engine, matcher, DSL, and event schema are **independent of this choice** — they
sit above the link endpoint — so Milestones 1–3 proceed against a `link/channel` endpoint
regardless.

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

### Runtime selection — RFC-046's two values, unchanged

**Resolved 2026-07-28 (open question 2): keep RFC-046's single `gvisor` value.**

An earlier draft of this RFC proposed splitting it into `gvisor-net` (packet gateway) and
`gvisor` (gateway + FS observation), on the grounds that FS observation requires runsc
while the packet gateway does not, and gating the cheap half behind the expensive half
would be a mistake.

The concern was right; the lever was wrong. **The cost is not the runtime, it is the
feature.** So there is one runtime, and the runsc requirement is driven by whether the spec
actually asks for filesystem observation:

| `runtime=` | Packet gateway | FS observation | Requires | Caps at |
|---|---|---|---|---|
| `"default"` | — | — | — | L1 |
| `"gvisor"` | ✅ always | ✅ when the spec calls `watch()` | `CAP_NET_ADMIN`; **runsc + container mode only if `watch()` is used** | L1 |

A spec that never calls `watch()` never needs runsc — which is exactly what the split was
protecting — without a second runtime name to explain or document. This also keeps RFC-046's
capability matrix intact rather than amending it.

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

**Shipped in v0.14.1:** two link-scoped shapers that take no matcher.

```python
bandwidth(rate="1mbit", dir="c2s", queue="250ms")
mtu(size=576)
```

These are not per-packet rules — they need a token bucket and real
fragmentation/PMTUD handling respectively, which is a different mechanism from the
match-and-act pipeline above. Shipping a `mtu()` that merely drops oversized packets would
look like a black hole without behaving like one, so scenario 8 uses
`packet_drop(len_gt=576)` in v0.14.0 and is documented as an approximation. Both land in
v0.14.1 as a focused follow-up.

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

10. ~~**WAL durability audit.**~~ **Withdrawn** — gVisor has no `fsync` trace point. See
    [Decision record M0.3](#decision-record-m03--runsc--seccheck). The nearest shippable
    substitute is a **WAL write-ordering audit**:
    `watch(pg, files=["**/pg_wal/*"], ops=["write"])` plus `assert_ordered(wal_write, http_200)`
    — proves the WAL was *written* before the commit was acknowledged, but not that it was
    *durable*.

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

1. ~~**Insertion mechanism**~~ — **RESOLVED**: candidate A (TUN + routed subnet).
   [Decision record M0.2](#decision-record-m02--insertion-mechanism).
2. ~~**`gvisor-net` as a distinct runtime value**~~ — **RESOLVED**: keep RFC-046's single
   `gvisor`. The runsc requirement is driven by `watch()` usage, not by a second runtime
   name. See §"Runtime selection".
3. **Payload cap for `Packet.payload`** — 4 KiB default proposed. Too small for
   payload-matching on large messages, too large to copy per packet at line rate. Should it
   be per-rule rather than global?
4. **`where=` lambda budget** — warn only, or fail the test past a threshold? Warning risks
   a user shipping a spec that is 100× slower than they think.
5. ~~**Does `runsc` availability gate spec load or service start?**~~ — **RESOLVED**: spec
   load. [Decision record M0.3](#decision-record-m03--runsc--seccheck).
6. **Do packet faults participate in `fault_matrix()` generation?** They have a much larger
   parameter space than syscall faults, and naive enumeration would explode the plan tree.
7. **Interaction with TLS (RFC-038).** Packet faults operate below TLS, so a corrupt on an
   encrypted stream produces a MAC failure rather than a semantic corruption. Is that the
   intended semantics, or should `packet_corrupt` refuse to target a TLS interface?

## Future work

Ordered. The first item is first because of a measurement, not a preference —
see below.

- **Fault-timing exploration — v0.14.1, ahead of the shapers.** `choose()` over
  *when* a fault lands (partition onset, hold duration) driven by `--explore`.
  This RFC delivered the ability to *express* a packet fault; it did not deliver
  the ability to *search* for the interleaving that triggers a bug, and those are
  different capabilities. The Raft harness made the gap concrete: 45 runs against
  the exact `hashicorp/raft` commit the Antithesis study reported bugs in, all
  green — but every run executed one identical fault schedule, because
  `--runs N` only varies a seed and nothing in that spec consumes a seed. See
  [the results write-up §4.2](../design/2026-07-28-raft-mesh-results.md). Until
  this lands, a green multi-run result is a flake check wearing the costume of a
  search.
  - Sibling, small: `--runs N` should say when the spec has no seed-sensitive
    construct. Silence there is what made the Raft result readable as evidence.
- ~~**`bandwidth(rate=)` and `mtu(size=)` — v0.14.1.**~~ **Shipped in v0.14.1.**
  `bandwidth()` paces via a single-server model whose queue is bounded in *time*
  (`queue="250ms"`), so it drops when saturated the way a real bottleneck does rather
  than buffering forever. `mtu()` overrides the link MTU, so netstack derives a smaller
  TCP MSS and fragments — a real small-MTU path, not the `packet_drop(len_gt=)`
  approximation scenario 8 had to use. Measured on the corpus: at 64kbit the c2s
  direction admitted 76 packets with a 38ms peak backlog; at 256kbit, 9ms.
- **`watch()` filesystem observation — [RFC-056](0056-filesystem-observation.md), v0.15.0.**
  The sink ships in v0.14.0 and is unchanged. A 2026-07-29 spike confirmed
  `-pod-init-config` fixes the tracing gap M5 measured (236 points on a network-driven
  query, versus 2; 11,295 at sandbox boot). What remains is that the flag is host-wide
  `daemon.json` state, which needs its own design — hence a separate RFC.
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
| Binary cost | Minimal main + the full netstack set = 6.5 MB standalone. **Actual measured cost once netfault is linked into `cmd/faultbox` (M2): +4.6 MB, 47.4 → 52.0 MB (+9.7%)** — below the standalone estimate, since the two binaries share runtime and stdlib |
| `go mod tidy` cost | ~4 min cold — gVisor's module graph is large. A CI cold-cache consideration, not a build-time one |

**No `nogvisor` build tag is needed.** The standing-risk fallback in the plan is withdrawn:
+13% is not worth a second build configuration and the maintenance it implies.

### Decision record M0.2 — Insertion mechanism

**Date:** 2026-07-28. **Status:** resolved. **Resolves open question 1.**

**Candidate A (TUN + routed subnet) is the fault gateway. Candidate B (veth + AF_PACKET) is
retained as a future opt-in *detection* mode.** Both were prototyped in the Lima VM
(kernel 6.8.0, Docker 29.3.1) against real containers.

#### What the spike proved

Both candidates deliver real traffic to a real `FaultEndpoint`, with headers parsed off
`*stack.PacketBuffer` exactly as the design assumes. Candidate A, VM host → netstack:

```
IN  tcp 10.99.0.1:58362 -> 10.99.0.5:8080 flags= S    seq=1525207598 win=64240 len=0
OUT tcp 10.99.0.5:8080 -> 10.99.0.1:58362 flags= S  A seq=4021171399 win=29184 len=0
IN  tcp 10.99.0.1:58362 -> 10.99.0.5:8080 flags=   PA seq=1525207599 win=502   len=82
*** ACCEPTED from 10.99.0.1:58362 ***
*** READ 82 bytes: "GET /hello HTTP/1.1\r\nHost: 10.99.0.5:8080..." ***
```

Every field the RFC's matcher needs — `dir`, `flags`, `seq`, `window`, `len` — is present
on a live connection. **A container reached netstack with no iptables changes at all**, which
contradicts this RFC's original prediction that Docker's `FORWARD policy DROP` would block
it; Docker 29's `DOCKER-FORWARD` chain already permits it.

#### The decisive difference: gateway vs tap

**Candidate B cannot drop a packet.** `AF_PACKET`/`ETH_P_ALL` *copies* frames the kernel has
already delivered — it observes, it does not divert. Turning B into a real gateway means
taking the container's veth peer out of `docker0`'s bridge and having netstack drive it,
which is substantially more container-network surgery than A.

Candidate A **terminates**: netstack owns the address the SUT dials, so Faultbox controls
the entire SUT-facing leg. That is the leg the SUT experiences, and it is what every
`packet_*` fault in this RFC acts on.

#### Source attribution — a concern that dissolved

With default Docker NAT, container traffic arrives masqueraded as `10.99.0.1`, losing the
sender's identity. Exempting the subnet from NAT *does* preserve the real source:

```
IN  tcp 172.17.0.2:35922 -> 10.99.0.5:8080 flags= S seq=3220560321 win=64240 len=0
```

…but then the SYN-ACK is dropped on the return path, because Docker's `FORWARD` chain only
accepts `in docker0`. Restoring connectivity needs **two** iptables rules, and Docker 29
rearranged these chains (`DOCKER-FORWARD` / `DOCKER-CT` / `DOCKER-BRIDGE`) relative to
older versions — a standing support burden.

**Faultbox does not need the source IP.** The proxy manager already assigns a distinct
listen *port* per interface; the gateway assigns a distinct netstack *address* per
`(consumer, target-interface)` pair. The destination then identifies both ends uniquely, so
`fault(db.main, source=worker)` targeting works with **zero firewall mutation**. The NAT
exemption is not adopted.

#### Comparison

| | **A — TUN + routed subnet** | **B — veth + AF_PACKET** |
|---|---|---|
| Traffic reaches netstack | ✅ | ✅ |
| **Can drop/delay/reorder** | ✅ terminates | ❌ **tap only** |
| Binary-mode services | ✅ | ❌ container-only |
| Firewall/NAT mutation | **none** | none |
| Source IP preserved | not needed (per-pair addresses) | ✅ natively |
| Sees traffic to hardcoded addresses | ❌ | ✅ **all** container traffic |
| Docker-version sensitivity | low (as adopted) | low |

#### Consequences

- **M4 implements candidate A.** Faultbox creates the TUN, assigns `10.99.0.0/16`, and
  allocates one address per `(consumer, target-interface)` pair. RFC-024's env-rewrite
  insertion contract is reused unchanged.
- **Candidate B is promoted to a future feature, not discarded.** It captures *all* container
  traffic with true source IPs and zero setup — precisely the RFC-024 env-rewrite bypass
  that is also L1's `network-unmediated` blind spot. As an observation-only mode it is a
  strong **detection** feature. Filed as future work, not v0.14.0 scope.
- **Noise:** a routed subnet also receives unrelated host traffic (mDNS to `224.0.0.251`
  was observed). The gateway filters to its own addresses; the event log must not surface
  host chatter as SUT traffic. Added as an M4 test.
- **Latency measurement deferred to M1**, where it can be done without the spike's
  per-packet `printf` dominating the result.

### Decision record M0.3 — runsc + seccheck

**Date:** 2026-07-28. **Status:** resolved. **Outcome:** Path C-lite is viable and adopted,
with **one scenario withdrawn**. **Resolves open question 5.**

Environment: Lima `faultbox-dev`, kernel 6.8.0, Docker 29.3.1,
`runsc release-20260721.0`, `postgres:16-alpine`.

#### Compatibility — the load-bearing question

**Postgres and Redis both run correctly under runsc.** `pg_isready` in 3 s; `CREATE TABLE`
/ `INSERT` / `SELECT` / `CHECKPOINT` all succeeded across 1000 rows; `redis-cli SET`/`GET`
round-tripped. `uname` inside the sandbox reports `4.19.0-gvisor`, confirming the Sentry.
The RFC-046 worry that "Postgres may break under gVisor" did not materialize for the
images Faultbox's own demos use.

Installation is non-invasive: `runsc install` adds a `runtimes` entry to
`/etc/docker/daemon.json`. **The default runtime stays `runc`** — nothing changes for
existing specs.

#### The sink works end-to-end

A `SOCK_SEQPACKET` UDS server completed the handshake and streamed points:

```
sink listening on /tmp/faultbox-seccheck.sock (SOCK_SEQPACKET, mode 0600)
*** sandbox connected ***
handshake in: version=1
handshake out: version=1 — streaming
```

The Sentry writes its `Handshake` first and reads the monitor's reply; after that the
monitor only reads, as the README specifies. 1054 points were captured in one run.

#### What the decoded points actually give us

Decoding a Postgres write workload produced exactly the `file_io` fields the RFC promises,
and one better than promised:

```
write points by sysno: map[64:5 68:263]        # 64=write, 68=pwrite64 (aarch64)
writes carrying has_offset: 263
  sysno=68 path=/var/lib/postgresql/data/pg_wal/000000010000000000000001 offset=7815168 count=8192  result=8192
  sysno=68 path=/var/lib/postgresql/data/pg_wal/000000010000000000000001 offset=7815168 count=81920 result=81920
  sysno=68 path=/var/lib/postgresql/data/pg_xact/0000                    offset=0       count=8192  result=8192

FAILED opens (real errno): 57
  pathname=/lib/libpq.so.5      errno=2
  pathname=/lib/libssl.so.3     errno=2

bytes written per path (top 3):
  /var/lib/postgresql/data/base/5/16403                     2080768 bytes
  /var/lib/postgresql/data/pg_wal/000000010000000000000001    98304 bytes
  /var/lib/postgresql/data/base/5/16409                       24576 bytes
```

- **Real byte offsets are populated** for positional I/O — `pwrite64` accounted for 263 of
  268 writes. Scenario 12 (write amplification / torn-record detection) is viable.
- **Per-path byte accounting works**, so `file_io().bytes_written` is directly implementable.
- **Failed opens carry the true errno**, which Faultbox has no equivalent of today.
- `pwrite64` shares `MESSAGE_SYSCALL_WRITE` and is distinguished by `sysno` — a decoder
  that switches on message type alone will silently conflate them.

#### Finding 1 — there is no `fsync` trace point. Scenario 10 is withdrawn

gVisor's seccheck covers 42 syscalls. `fsync`, `fdatasync`, `msync`, and `sync_file_range`
are **not among them**:

```
accept accept4 bind chdir chroot clone close connect dup dup3 eventfd2 execve execveat
fchdir fcntl inotify_add_watch inotify_init1 inotify_rm_watch mmap openat pipe2 pread64
preadv preadv2 prlimit64 pwrite64 pwritev pwritev2 read readv setgid setresgid setresuid
setsid setuid signalfd4 socket socketpair timerfd_create timerfd_gettime timerfd_settime
write writev
```

**Scenario 10 (WAL durability audit — "prove fsync precedes the 200") cannot be built on
Path C-lite** and is withdrawn from v0.14.0. Options considered:

- *Merge seccomp-notify for fsync.* Path A already traces `fsync`/`fdatasync`. But under
  `runtime="gvisor"` the SUT runs inside the Sentry, and `SECCOMP_RET_USER_NOTIF` support
  in the guest is unverified. Not adopted without its own spike.
- *Contribute an fsync point upstream.* Correct long-term; not a v0.14.0 dependency.
- **Adopted:** `watch(ops=[...])` rejects `"fsync"` at spec load under `runtime="gvisor"`
  with an error naming the limitation, rather than silently emitting nothing.

`mmap` *is* traced, which partially closes the "mmap'd I/O is invisible" gap this RFC lists
under Path A — we see the mapping, though not per-page faults.

#### Finding 2 — `openat` path resolution needs assembly

`Open.pathname` is the **raw syscall argument**, which is frequently relative
(`global/pg_filenode.map`, `base/5/PG_VERSION`), while `Read`/`Write.fd_path` is the
Sentry-**resolved absolute** path. Resolving an `openat` therefore means combining
`Open.fd_path` (the dirfd's path) with `pathname`, falling back to the `cwd` context field
for `AT_FDCWD`. The `cwd` context field must be requested explicitly.

Practical consequence: **`watch(files=)` matching should key on `fd_path` from
read/write/close**, which is unambiguous, and treat `openat` as a secondary signal.

#### Finding 3 — session naming and fixtures

- Trace sessions **must be named `Default`**; any other name fails with *"only a single
  \"Default\" session is supported"*.
- `fd_path` is an **optional field** and must be requested per point, or it arrives empty.
- **Fixtures work.** 392 captured messages were decoded on **darwin/arm64 with no runsc and
  no Linux**, recovering paths, offsets, byte counts, and errnos. The plan's claim that the
  M5 decoder is fully testable on a macOS host is now demonstrated rather than assumed.

#### Consequences

- Path C-lite is adopted for M5.
- **Scenario 10 is withdrawn**; the scenario list for v0.14.0 is 11, not 12.
- Open question 5 resolves to **spec load**: runsc availability is checked when a spec
  declares `runtime="gvisor"`, since the error is far more actionable there.
- M5 gains three tests from these findings: `sysno` disambiguation of write/pwrite64,
  `openat` path assembly from dirfd + pathname + cwd, and a spec-load rejection of
  `ops=["fsync"]`.

### Decision record M5 — Filesystem observation deferred to v0.14.1

**Date:** 2026-07-28. **Status:** resolved. **Outcome:** `watch()` is **withdrawn from
v0.14.0**. The sink ships; the primitive does not.

#### What works

`internal/gvisor/seccheck` is complete: the SOCK_SEQPACKET server, handshake, wire framing,
protobuf decode, and both M0.3 corrections (pwrite64 vs write by `sysno`; `openat` path
assembly from dirfd + pathname + cwd). A real Postgres stream captured under runsc is
committed as a fixture and decodes on darwin/arm64 with no runsc and no Linux — 102 writes
(97 positional with true byte offsets), 120 opens (36 failed with real errno), 46 paths.
The `watch()` DSL, `file_io` event schema, and path matching are implemented and tested.

#### What does not

**`runsc trace create` instruments only tasks created *after* the session starts.**

Faultbox attaches a trace session once a service is up and healthchecked — by which point
every worker thread already exists, so none of them is instrumented. Measured in the Lima
VM against `postgres:16-alpine`:

| Workload driving the same SQL | Trace points |
|---|---|
| Network query to the already-running backend | **2** |
| `docker exec psql` — a newly spawned process | **1054** |

The M0.3 spike used the second shape, which is why the limitation did not surface then.
That is a genuine gap in the spike, not a change upstream.

#### Why it is withdrawn rather than shipped with a caveat

A `watch()` that observes almost nothing still *runs*, and every assertion under it still
*passes*. An I/O-surface audit asserting "this service never writes outside its data
directory" would go green having seen two operations. That is precisely the failure mode
this release exists to eliminate — the same reasoning that makes a packet fault under
`runtime="default"` a hard error rather than a warning. A documentation caveat does not fix
a vacuous green test.

So `watch()` fails at spec load in v0.14.0, naming the limitation and the release it lands
in. Packet faults are unaffected.

#### The fix, for v0.14.1

runsc's `-pod-init-config` installs trace sessions at sandbox boot, so every task is
instrumented from the first instruction. It is a **runtime-level** flag registered in
`daemon.json` (`runtimeArgs`), not a per-container option, so it needs its own design:
the config is host-wide and shared by every runsc container, which interacts badly with
concurrent runs and with users who already run gVisor for other reasons. That design is
its own design. **Resolved 2026-07-29:** the mechanism works — see the
[spike](../design/2026-07-29-pod-init-config-spike.md) — and the remaining host-config
problem is specified in [RFC-056](0056-filesystem-observation.md), target v0.15.0.

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
