# RFC-056: Filesystem Observation — `watch()` on gVisor Trace Sessions

> **Status: Proposed.** 2026-07-29. Target: **v0.16.0**.
> Completes the half of [RFC-054](0054-gvisor-packet-and-file-mediation.md) that was
> withdrawn from v0.14.0 (decision record M5). The tracing mechanism is settled by the
> [`-pod-init-config` spike](../design/2026-07-29-pod-init-config-spike.md); what this RFC
> designs is the host-configuration lifecycle around it.
> Uses the L0–L5 vocabulary from [RFC-040](0040-determinism-levels.md).

## Summary

RFC-054 built filesystem observation end to end — the seccheck sink, the `watch()` DSL,
the `file_io` event schema, path matching — and then **refused to ship it**, because the
tracing mechanism available at the time observed almost nothing. `watch()` fails at spec
load in v0.14.0 and v0.14.1, naming the limitation.

A spike has since established that the mechanism is fine; the wrong one was being used.
`runsc trace create` attaches a session to a running sandbox and instruments only tasks
created afterwards. `runsc --pod-init-config` installs the session at sandbox boot, so
every task is traced from its first instruction:

| Workload | `trace create` | `-pod-init-config` |
|---|---|---|
| Sandbox boot, before any query | — | **11,295 points** |
| Network query to the running backend | **2** | **236** |

This RFC therefore proposes **no change to the tracing design and no change to the
decoder**. Both shipped in v0.14.0 and are unmodified by the spike. What it designs is
the part the spike exposed as the real problem: `-pod-init-config` is a **runtime-level
flag in `daemon.json`**, which makes the sink path host-wide state that concurrent runs
collide on.

That is a smaller uncertainty and a larger plumbing problem than RFC-054 deferred, which
is why it is a release rather than a patch.

The proposed resolution is to stop asking the host to enforce honesty. Configure the trace
session to **tolerate** an absent sink, so a registration left on an idle machine is inert,
and enforce "this run observed nothing" in Faultbox — which already does exactly that for
packet faults, and has caught two real false passes doing it.

## Motivation

### What users cannot express today

Faultbox can say "this service made 4,000 `write` calls". It cannot say **which file**,
except through a path recovered out-of-band from `/proc` — which races the SUT, truncates
at 256 bytes, and matched with a single-segment glob until v0.14.1. The gap shows up as
questions that sound basic and are currently unanswerable:

- Did this service write outside its data directory?
- Did the migration `fsync` before reporting success?
- Which files does the SUT touch on a cold start that it does not touch on a warm one?
- Did the config reload actually re-read the config, or serve a cached copy?

RFC-054 §"Verified ground truth" specified `watch()` for exactly these, as
**observation-only** — no fault injection. That scope is unchanged here and the reasoning
is unchanged: the fault vocabulary for filesystems (short writes, torn writes, `fsync`
lies, bit-rot) should be designed once, against a datapath that can implement it, which
is FUSE and not tracing. See "Non-goals".

### Why this is not a v0.14.x patch

The v0.14.0 decision record said the fix "needs its own design: the config is host-wide
and shared by every runsc container, which interacts badly with concurrent runs and with
users who already run gVisor for other reasons."

The spike confirmed all three concerns and sharpened them into concrete failure modes:

1. **The sink path is baked into host config.** Every run must agree on one socket path,
   or rewrite `daemon.json` and restart the Docker daemon per run — slow, and hostile on
   a shared machine.
2. **Concurrent runs collide.** Two runs pointing one runtime at different sockets cannot
   coexist. This is v0.14.1's `faultbox0` bug one layer up: shared global state named
   without regard to who owns it. That precedent is instructive — the fix there was
   per-process naming plus orphan reaping, and the same shape is likely correct here.
3. **`ignore_setup_error: false` means the sandbox refuses to boot** without a live sink.
   This is the right default — a silently untraced run is exactly what v0.14.0 refused to
   ship — but it means a stale registration breaks **every** gVisor container on the host,
   including ones unrelated to Faultbox.

Point 3 is the one that makes this a design problem rather than a plumbing task: the
honest configuration is also the dangerous one. The proposed design reconciles it by
moving the honesty requirement off the host and into Faultbox.

## Verified ground truth

Everything below is measured, not assumed. Sources: RFC-054 decision records M0.3 and M5,
and the [2026-07-29 spike](../design/2026-07-29-pod-init-config-spike.md).

| Claim | Evidence |
|---|---|
| The seccheck decoder is correct and complete | 11,531 points decoded, **0 errors, 0 drops**; committed Postgres fixture decodes on darwin/arm64 with no runsc and no Linux |
| `-pod-init-config` instruments pre-existing tasks | 11,295 points before any query reached the sandbox |
| A network-driven workload is observed | 236 points vs 2 with `trace create`, same image, same host, same SQL |
| The flag exists but is undocumented | Absent from `runsc --help`; a bogus-flag control confirms it is parsed, not ignored |
| `pwrite64` is distinguishable from `write` | By `sysno`, with true byte offsets (M0.3) |
| `openat` paths reconstruct correctly | From dirfd + pathname + cwd (M0.3) |
| gVisor has no `fsync` trace point | M0.3 — see "Open questions" |
| `ignore_setup_error: true` + **absent** sink | Container boots normally, untraced — a stale registration is inert, not a landmine |
| `ignore_setup_error: true` + **present** sink | Tracing works unchanged; points arrive as usual |

The last two are what make the proposed design possible and were measured on 2026-07-29
specifically to check it, because the design rests entirely on them.

| Traced-but-unwatched overhead | **~18%** on a 280 k points/sec workload; **zero** when the runtime is registered but idle |
| Sink drop threshold | **0 drops below ~17 k points/sec; drops appear by ~47 k/s** |

The last two were measured on 2026-07-29 as milestone 0 (see the
[implementation plan](../implementation/v0.16.0-rfc-056-plan.md)). They settle the
open design branch — **the runtime is registered broadly, not opt-in per service** —
because the idle cost is nil and the active cost is bounded and only reached by
workloads that are themselves unusual.

The drop threshold was not anticipated by this RFC and changes it; see
"Dropped points are a correctness boundary" below.

## Non-goals

- **Filesystem fault injection.** `watch()` observes. Short writes, torn writes, `fsync`
  lies, `ENOSPC`-after-N-bytes need a datapath that can *change* what the SUT sees; a
  trace point fires after the syscall has already happened. That is the FUSE work in
  RFC-054's future list, and designing the fault vocabulary before that datapath exists
  would produce a vocabulary the datapath cannot implement.
- **Binary-mode services.** Trace sessions are a sandbox property. A host binary under
  seccomp-notify has no sandbox, so `watch()` is container-only and must say so at spec
  load rather than silently observing nothing — the v0.14.0 lesson.
- **Replacing `/proc` path recovery** for syscall faults. Those are two mechanisms with
  different costs; `watch()` does not make `op(path=)` obsolete.
- **Tracing every point gVisor offers.** The scope is file I/O: `openat`, `write`,
  `pwrite64`, and whatever "Open questions" resolves for close/read.

## Proposed design

### The shape of the problem

```
faultbox run ──> needs a UDS sink at a path only it knows
                                │
                                ▼
                    daemon.json runtimeArgs        ← host-wide, one value
                    --pod-init-config=/some.json
                                │
                                ▼
                    every runsc container on the host
```

The sink path must reach the sandbox through host configuration, but it is per-run data.

Two constraints follow, and they shape everything below:

1. **Host setup must be one-time and explicit.** A test run that rewrites `daemon.json`
   and restarts the Docker daemon would kill every unrelated container on the machine.
   Whatever the design, per-run it must touch no host state.
2. **A stale registration must not break the host.** With `ignore_setup_error: false` —
   the setting the spike used — a sandbox refuses to boot when the sink is absent. Between
   Faultbox runs there *is* no sink, so a one-time registration would stop **every** gVisor
   container on that machine, including ones unrelated to Faultbox.

Constraint 2 is the hard one, because the honest setting is the dangerous one.

### Proposed: tolerate the missing sink, and fail loudly on our side

Set **`ignore_setup_error: true`** and move the honesty requirement out of the host
config and into Faultbox, where it already has a proven home.

**Measured 2026-07-29**, both halves:

| Sink | `ignore_setup_error` | Result |
|---|---|---|
| absent | `true` | container **boots normally**, untraced |
| present | `true` | tracing **works** — points arrive as usual |

So a registration left on a machine with no Faultbox running is inert, not a landmine.
Unrelated gVisor containers are unaffected whether or not a sink exists.

That trade would be unacceptable on its own — a run could now be silently untraced, which
is precisely why v0.14.0 withdrew `watch()` rather than shipping it with a caveat. What
makes it acceptable is that Faultbox already has the guard, and it is proven:

```
packet faults were installed 8 time(s) but no netstack gateway was attached,
so no packet was affected; the result below would be meaningless
```

That check (`packetRuleRegistry.unwiredInstalls`) turned a run of 18 leaves that all
reported success into a loud failure, twice during v0.14.1's development — once from a
leaked TUN device, once from a leaked one under a different cause. The same shape applies
directly: **`watch()` was installed, zero trace points arrived → fail the test.** The
spec author cannot get a vacuous green either way; the difference is only whether the
enforcement lives in the host's config or in ours.

Ours is better placed. It knows whether this run asked for observation, and the host does
not.

### Dropped points are a correctness boundary

Milestone 0 measured the sink losing points above roughly 17,000 per second:

| Trace points | Rate | Dropped |
|---|---|---|
| 4,910 | 17 k/s | 0 |
| 19,927 | 47 k/s | 76 |
| 104,212 | 280 k/s | 94 |

`seccheck.Sink.Dropped()` already exists, and its v0.14.0 comment anticipated why it
would matter: *"Non-zero means the sink could not keep up and the observation is
incomplete — which must be surfaced, not averaged away."*

That is now load-bearing rather than defensive. The canonical use of `watch()` is an
audit — *this service never writes outside its data directory* — and a dropped point
could be the violating one. An audit that missed 76 operations cannot claim "never"; it
can only claim "never, among those I saw", which is not the assertion the author wrote.

**So a non-zero drop count fails the test**, by the same rule and for the same reason as
`watch()` installed with zero points observed. Both are cases where the run saw less than
it claims to have seen, and neither is a number to tune quietly.

### The default trace point set must be narrow

RFC-054's `FileIOPoints()` requests **eight** points: `openat`, `write`, `pwrite64`,
`writev`, `read`, `pread64`, `close`, `connect`. That was written before drops were known
to matter. Measured on a read-heavy workload — write 20 MB, read it back four times:

| Trace points requested | Points delivered | **Dropped** |
|---|---|---|
| 3 — `openat`, `write`, `pwrite64` | 25,015 | **0** |
| 8 — the RFC-054 default | 48,576 | **1,488** |

Reads roughly double the volume, and the drop count goes from zero to fifteen hundred on
a workload that is not extreme. Under the rule above — drops fail the test — **the
inherited default would make `watch()` fail on ordinary read-heavy services.**

So the shipped default is the narrow set: `openat`, `write`, `pwrite64`, `writev`.
`read`, `pread64`, `close` and `connect` are **opt-in at `setup-trace` time**, with this
measurement as the stated reason.

Two consequences for the implementation:

- **The DSL must reject ops the installed session cannot deliver.** `watchableOps`
  already accepts `read` and `close`; if the host's trace config does not request them, a
  `watch(ops=["read"])` would observe nothing and its assertions would pass vacuously —
  the precise failure v0.14.0 withdrew `watch()` to avoid, reintroduced through the back
  door. It must fail at spec load naming the fix
  (`faultbox setup-trace --with-read`), exactly as `ops=["fsync"]` already fails naming
  gVisor's missing trace point.
- **Volume reaching the event log is already bounded.** `onFileIO` discards any operation
  no active `watch()` matches before it becomes an event, so `watch(files=["/var/lib/**"])`
  costs report size proportional to what the spec asked about, not to what the service
  did. The cost this section is about is upstream of that: points that cross the socket
  only to be discarded, and the drops they cause.

The division of labour: **the spec author declares what matters** (`files=`, `ops=`), and
the host config is the conservative floor that keeps the channel honest. Neither should
try to capture everything.

### What the user does, once

```json
"faultbox-trace": {
  "path": "/usr/local/bin/runsc",
  "runtimeArgs": ["--pod-init-config=/etc/faultbox/trace.json"]
}
```

Plus one daemon restart, at install time, and only for users of `watch()`. Packet faults,
syscall faults and the proxies need none of it. Per run, Faultbox writes no host state at
all: it binds its sink at the configured path, runs, and unbinds.

### Concurrency, deliberately deferred

Two runs on one host both want the same socket path. The second `bind` fails, its
containers boot untraced, and the guard fails that run with a clear message naming the
cause. That is a correct outcome, if a blunt one, and it needs no extra machinery.

Routing by `container_id` — which every trace point already carries, and which the decoder
already parses — would let concurrent runs share one sink. That is a worthwhile follow-on
and explicitly **not** a precondition: it buys throughput, not correctness, and pricing a
resident multiplexer process into v1 to buy throughput nobody has asked for yet is the
wrong trade.

### What does not change

- `internal/gvisor/seccheck` — sink, framing, protobuf decode, path assembly. Unmodified.
- The `watch()` DSL surface as specified in RFC-054 §"DSL extensions".
- The `file_io` event schema.
- `internal/pathmatch` (shipped v0.14.0, already used by syscall path faults).

The implementation is therefore mostly *deletion* of the v0.14.0 spec-load refusal, plus
the multiplexer and its lifecycle.

## Determinism impact

**None.** `watch()` observes; it changes no scheduling decision and injects nothing. The
determinism ceiling stays L1, exactly as `runtime="gvisor"` does not raise it in v0.14.0.

What widens is the *observed* surface, which is the same distinction RFC-054 drew for
packet faults: the mediated surface grows, the promise does not.

One caveat that must be documented rather than smoothed over: enabling tracing has a
runtime cost inside the sandbox, so a spec that adds `watch()` may perturb timing enough
to change whether a *different*, timing-sensitive assertion holds. Observation is not
free and should not be described as if it were.

## Impact

- **Breaking:** none. `watch()` currently fails at spec load; specs using it do not exist.
- **New host requirement:** a `daemon.json` runtime registration, written by
  `faultbox init` (or an explicit `faultbox setup-trace`) rather than silently by a test
  run. Modifying a user's Docker daemon configuration is not something a test run should
  do without being asked.
- **Docs:** `watch()` moves from "fails at spec load, lands in v0.14.1" to a documented
  primitive, with the container-only limitation stated at the point of use.

## Alternatives considered

- **A resident sink multiplexer** (the first draft's recommendation). One long-lived
  process owning the socket permanently, routing by `container_id`, so the endpoint is
  always present and `ignore_setup_error: false` stays safe. Superseded: measuring
  `ignore_setup_error: true` showed the socket does not need to be always present, which
  removes the entire reason for the resident process. Its routing idea survives as the
  concurrency follow-on above.
- **Per-run runtime registration** — write `faultbox-trace-<pid>` into `daemon.json` at
  run start, restart the daemon, remove it at teardown. Fully isolates concurrent runs,
  and disqualified by the restart: seconds of latency per run, and it disrupts every other
  container on the host.
- **A fixed socket path with `ignore_setup_error: false`** and a lock file to serialise
  runs. Simple, and it leaves the landmine: any period with no Faultbox running breaks
  every gVisor container on the machine.

- **Ship `watch()` with a caveat in v0.14.0.** Rejected then and still rejected: a
  `watch()` that observes 2 of 1054 operations still *runs*, and every assertion under it
  still *passes*. An audit asserting "this service never writes outside its data
  directory" would go green having seen two operations.
- **Poll `/proc/<pid>/fd` instead of tracing.** Cheap, no gVisor requirement, and wrong:
  it samples rather than observes, so it cannot answer "did this ever happen" — only
  "was this true when I looked".
- **eBPF.** Faultbox's premise is seccomp-notify precisely to avoid an eBPF toolchain
  dependency (see RFC-046). Adding one for filesystem observation would undo that for a
  single feature.
- **Fork runsc to accept a per-container flag.** RFC-054's Path C, rejected there and
  here: a fork is a permanent maintenance cost, and the spike shows the stock flag works.

## Open questions

1. ~~**Does `close` matter?**~~ **Resolved:** available, but off by default and opt-in via
   `setup-trace`, on the volume evidence above. "Opened and never closed" stays
   expressible for those who turn it on and accept the cost.
2. ~~**`read` points**~~ **Resolved:** off by default. Measured at ~2× volume and 1,488
   dropped points on a read-heavy workload, which under the drop rule would fail the test
   outright. Opt-in, with the measurement as the documented reason.
3. **`fsync` has no trace point** (M0.3). Durability auditing — RFC-054's scenario 10 —
   stays impossible. Is that worth an upstream contribution to gVisor?
4. **Multiplexer overhead on unclaimed containers.** How much does a traced-but-unwatched
   container pay? This bears on candidate C directly.
5. **How does `watch()` compose with `--explore` fan-out**, where a spec runs 24 times and
   each leaf starts fresh containers?
6. **Should `faultbox init` write the runtime registration, or a separate opt-in command?**
   Related: what does Faultbox do when it finds a *stale* registration pointing at a
   socket nobody serves?

## Future work

- **FUSE datapath for filesystem fault injection.** The natural successor, and the reason
  `watch()` is observation-only.
- **`fsync` trace point upstream**, if question 3 resolves that way.
- **Byte-level content assertions** — "the WAL record written at offset N had this shape"
  — which the decoder already has the offsets for.

## Dependencies

- **[RFC-054](0054-gvisor-packet-and-file-mediation.md)** — built the sink, the DSL, and
  the event schema; withdrew the primitive. This RFC completes it.
- **[RFC-040](0040-determinism-levels.md)** — the `fs-unmediated` category and the L0–L5
  vocabulary.
- **[RFC-046](0046-beyond-l1-roadmap.md)** — Path C-lite, of which this is the delivery.
