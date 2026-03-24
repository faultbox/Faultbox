# PoC Step 5: Filesystem Fault Injection

**Branch:** `poc/step-5-filesystem-faults`
**Status:** Complete
**Date:** 2026-03-24

## Executive Summary

### What

Complete filesystem fault injection by adding the remaining capabilities that
Steps 3-4 didn't cover: **stateful faults** ("fail the Nth write"), **fsync
targeting**, and an **`--fs-fault` convenience flag** that maps to the right
syscalls automatically.

### Reassessment: FUSE vs Seccomp-Only

The original plan for Step 5 was a FUSE passthrough filesystem. After research
and reviewing what Steps 3-4 already deliver, FUSE is **not justified** for the
PoC:

**What we already have via seccomp (Steps 3-4):**

| Capability | How | Example |
|---|---|---|
| Fail file opens | `--fault "openat=ENOENT:100%:/data/*"` | Step 3 |
| Fail writes with EIO/ENOSPC | `--fault "write=EIO:100%"` | Step 2 |
| Fail fsync | `--fault "fsync=EIO:100%"` | Step 2 |
| Slow file I/O | `--fault "write=delay:200ms:100%"` | Step 4 |
| Path-targeted faults | `--fault "openat=ENOENT:100%:/data/*"` | Step 3 |
| System path exclusion | Automatic for /lib, /proc, /sys | Step 3 |

**What FUSE would add:**

| Capability | Value for P-lang | Complexity |
|---|---|---|
| Data corruption (modify read buffer) | Low — P-lang models failures, not corruption | High |
| Partial reads/writes | Low — not a P-lang concern | High |
| Per-file-instance state | Medium — useful but achievable via seccomp counters | High |

**FUSE complexity cost:**
- Two-process shim model (daemon + target)
- `/dev/fuse` access inside user namespace
- Daemon lifecycle management and deadlock avoidance
- New dependency: `hanwen/go-fuse` v2
- Target binary must NOT be on the FUSE mount (exec deadlock)

**Decision: Skip FUSE, enhance seccomp.** The effort-to-value ratio doesn't
justify FUSE for the PoC. We can add it later if data corruption scenarios
become a requirement. Instead, Step 5 focuses on:

1. **Stateful faults** — "fail after N calls" or "fail the Nth call"
2. **`--fs-fault` convenience syntax** — maps to the right seccomp syscalls
3. **Counters in the notification loop** — per-path call counting

### Why This Is Step 5

Even without FUSE, there's a gap between "every openat to /data fails" and
"the 3rd fsync to /data/wal fails after the commit ack." The latter is what
P-lang traces actually produce. Stateful faults close this gap.

| P-lang trace says | Current capability | Step 5 capability |
|---|---|---|
| "write to WAL fails" | `--fault "write=EIO:100%"` — all writes fail | Same (works) |
| "3rd fsync fails" | Can't do — probability only | `--fault "fsync=EIO:100%:after=2"` |
| "first open succeeds, second fails" | Can't do | `--fault "openat=ENOENT:100%:nth=2"` |
| "write fails after 1MB written" | Can't do | Future: byte-count triggers |

### Expected Outcome

Extended fault rule syntax with stateful triggers:

```bash
# Fail the 3rd fsync (first 2 succeed, 3rd returns EIO)
faultbox run --fault "fsync=EIO:100%:nth=3" ./my-service

# Fail all fsyncs after the 5th one succeeds
faultbox run --fault "fsync=EIO:100%:after=5" ./my-service

# Classic WAL test: fail fsync on /data/wal after 2 successful syncs
faultbox run --fault "fsync=EIO:100%:after=2:/data/*" ./my-service

# Convenience: same as --fault "openat=ENOENT:100%:/data/*"
faultbox run --fs-fault "open=ENOENT:100%:/data/*" ./my-service
```

### Technical Approach

#### Stateful Fault Triggers

New optional trigger parameters appended to the fault rule:

```
--fault "SYSCALL=ACTION:PROB%[:PATH_GLOB][:TRIGGER]"
```

Triggers:
- `nth=N` — fire only on the Nth matching call (1-indexed). Calls before N are
  allowed. After firing once, the rule is inactive.
- `after=N` — allow the first N matching calls, then fire on all subsequent.

Implementation: each rule gets a per-rule atomic counter. The notification
handler increments the counter on match and checks against the trigger before
applying the fault.

```go
type FaultRule struct {
    // ... existing fields ...
    Trigger     FaultTrigger  // none, nth, after
    TriggerN    int           // the N value
    counter     atomic.Int64  // per-rule call counter (thread-safe)
}
```

#### `--fs-fault` Convenience Flag

Maps filesystem operation names to the correct syscall(s):

| fs-fault op | Maps to syscall(s) |
|---|---|
| `open` | `openat` |
| `read` | `read`, `readv` |
| `write` | `write`, `writev` |
| `sync` / `fsync` | `fsync` |
| `mkdir` | `mkdirat` |
| `delete` | `unlinkat` |
| `stat` | `fstatat` |

This is purely syntactic sugar — under the hood it creates regular fault rules.

### Implementation Plan

1. **Add FaultTrigger to FaultRule** — `nth=N` and `after=N` trigger types,
   atomic counter per rule.

2. **Update parser** — recognize `:nth=N` and `:after=N` at the end of the
   fault rule string.

3. **Update notification handler** — increment counter on match, check trigger
   before applying fault.

4. **Add `--fs-fault` flag** — convenience mapping in CLI.

5. **Test** — verify stateful faults fire at the right time, counter is
   thread-safe, fsync targeting works.

### Non-Goals for Step 5

- No FUSE filesystem (see rationale above)
- No data corruption (requires FUSE or ptrace — deferred)
- No partial reads/writes
- No byte-count triggers ("fail after 1MB written")
- No per-fd tracking (counters are per-syscall-name, not per-file-descriptor)

---

## Findings

Tested on Lima VM (Ubuntu 24.04, kernel 6.8.0-101-generic, Go 1.26.1).

### What Worked

- **Atomic counters for stateful triggers:** `sync/atomic.Int64` provides
  thread-safe counting across concurrent goroutine-per-notification handlers.
  `ShouldFire()` increments and checks in a single atomic operation.

- **`nth=N` fires exactly once:** Counter reaches N, fires, then all subsequent
  calls have counter > N and don't match. Clean one-shot behavior.

- **`after=N` fires on all calls after N:** First N calls are allowed (counter
  1..N), then counter > N fires on every subsequent call. The classic "allow
  some then fail" pattern.

- **Triggers compose with path globs and delays:** `openat=delay:500ms:100%:/tmp/*:nth=2`
  works correctly — only the 2nd openat to `/tmp/*` is delayed.

- **`--fs-fault` convenience is pure syntactic sugar:** Maps operation names to
  syscalls at CLI level, then feeds into the existing `--fault` parser. Zero
  engine changes needed.

- **FUSE correctly deferred:** All filesystem fault injection goals achievable
  via seccomp + stateful triggers. FUSE adds data corruption but at high
  complexity cost — not justified for the PoC.

### Architecture Delivered

```
internal/engine/fault.go         → FaultTrigger (TriggerNth, TriggerAfter)
                                    ShouldFire() with atomic counter
                                    Updated parser for :nth=N and :after=N
cmd/faultbox/main.go             → --fs-fault flag with expandFsFault() mapping
```

### Connection to P-lang

Stateful triggers directly map to P-lang verification traces:

```
P trace: "system accepts 2 writes, then fsync fails"
  → --fault "fsync=EIO:100%:after=2"

P trace: "3rd connection attempt times out"
  → --fault "connect=ETIMEDOUT:100%:nth=3"

P trace: "WAL write succeeds, commit fsync fails"
  → --fault "fsync=EIO:100%:nth=1"
```

This is the bridge between formal model checker output and real fault injection.

---

## Connection to Roadmap

```
Step 5 (this) → stateful faults + fs-fault convenience
  ↓
Step 6 → architecture review: multi-service topology
Step 7 → network message ordering (P-spec replay)
  - Stateful triggers from Step 5 enable "fail the 3rd message" scenarios
  ↓
Steps 8-10 → formal modeling integration
  - P-lang traces translate directly to stateful fault rules
```
