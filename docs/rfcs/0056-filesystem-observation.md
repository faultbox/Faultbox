# RFC-056: Filesystem Observation — `watch()` on gVisor Trace Sessions

> **Status: Proposed.** 2026-07-29. Target: **v0.15.0**.
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
honest configuration is also the dangerous one, and the RFC has to reconcile that.

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

**The one thing not yet measured** is whether the per-run daemon reconfiguration this RFC
proposes is fast enough to be tolerable. That is the first implementation milestone, and
it gates the rest.

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
Three candidate resolutions.

### Candidate A — dedicated runtime, stable socket directory

Register one runtime, `faultbox-trace`, pointing at a **fixed** pod-init config whose sink
endpoint is a **fixed** path (`/run/faultbox/seccheck.sock`). Each run binds that path;
concurrent runs are serialised by a lock file.

- **Pro:** `daemon.json` is written once, at `faultbox init`, never per run. No daemon
  restarts on the hot path.
- **Con:** concurrent runs cannot both trace. Acceptable for a laptop, wrong for CI.
- **Con:** a crashed run leaves a dead socket and the next sandbox refuses to boot —
  needing exactly the orphan-reaping logic v0.14.1 added for TUN devices.

### Candidate B — per-run runtime registration

Write `faultbox-trace-<pid>` into `daemon.json` at run start, restart the daemon, remove
it at teardown.

- **Pro:** concurrent runs are fully isolated, by construction.
- **Con:** a Docker daemon restart per run. Cost unmeasured; likely seconds, and it
  disrupts every other container on the host. Probably disqualifying, but it should be
  measured rather than assumed.

### Candidate C — sink multiplexer (**recommended**)

One long-lived registration as in A, but the fixed socket is served by a **demultiplexer**
that routes points to the run that owns the container.

Each trace point carries `container_id` in its context fields — already requested by
`FileIOPoints()` and already decoded. A small resident sink accepts every sandbox's
connection and forwards each point to whichever run has registered an interest in that
container ID.

- **Pro:** concurrent runs work, with no daemon restart on the hot path.
- **Pro:** the socket is always present, so the `ignore_setup_error: false` hazard —
  a stale registration breaking unrelated gVisor containers — disappears. Points for
  containers nobody claims are simply discarded.
- **Con:** a resident process, with its own lifecycle, upgrade story, and failure modes.
- **Con:** points for unclaimed containers cross the socket and are dropped. That is
  overhead the user did not ask for on containers they did not fault, which needs
  measuring against the "unfaulted services run at native speed" principle.

**Recommendation: C**, with A as the fallback if the resident process proves unjustified.
C is the only candidate where the honest configuration (`ignore_setup_error: false`) is
also the safe one, and that reconciliation is the central problem this RFC exists to
solve.

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

1. **Does `close` matter?** Without it, a "file was opened and never closed" assertion is
   inexpressible. gVisor has the point; the cost is more traffic on the sink.
2. **`read` points** — RFC-054 scoped to writes and opens. Reads would make "did the
   config reload actually re-read the file" expressible, at a large volume increase.
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
