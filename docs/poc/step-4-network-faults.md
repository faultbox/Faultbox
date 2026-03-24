# PoC Step 4: Network Fault Injection

**Branch:** `poc/step-4-network-faults`
**Status:** Complete
**Date:** 2026-03-24

## Executive Summary

### What

Add network-level fault injection: latency, connection failures, and selective
blocking — all via the existing seccomp-notify mechanism. No new kernel
privileges, no veth pairs, no tc/netem. The seccomp notification loop already
pauses the target's syscalls; we just need to sleep before responding.

### Why This Is Step 4

Network failures are the bread and butter of distributed systems bugs:

| Failure mode | Real-world cause | What breaks |
|---|---|---|
| Connection refused | Service crash, port conflict | Retry logic, circuit breakers |
| Connection timeout | Network partition, overloaded peer | Timeout handling, fallback paths |
| Latency spike | GC pause, network congestion | Request pipelining, cascading timeouts |
| Partial failure | Some calls succeed, some fail | Consistency, idempotent retries |

Step 2 already lets us reject syscalls with errno. Step 4 adds **time** as a
fault dimension — making the target experience realistic network conditions
rather than instant-fail-or-succeed.

### Why Seccomp-Based (Not tc/netem)

| Approach | Requires | Gives us |
|---|---|---|
| **tc/netem via veth** | `CAP_NET_ADMIN` on host + veth pair setup | Packet-level: latency, loss, reorder, corruption |
| **seccomp delay** | Nothing new (already have the notification fd) | Syscall-level: latency on connect/send/recv, reject, timeout simulation |

tc/netem is the gold standard but requires host-level `CAP_NET_ADMIN` to create
veth pairs and move them into the child's namespace. Our unprivileged
`CLONE_NEWUSER` launch can't do that.

Seccomp delay is sufficient for the PoC because:
1. **P-lang operates at message granularity** — it models `send(msg)` and
   `recv(msg)`, not individual packets. Seccomp intercepts at exactly this level.
2. **Connect/send/recv delays simulate real network conditions** — a 200ms delay
   on `connect()` is indistinguishable from a slow network to the application.
3. **No new dependencies** — works with the existing notification loop.
4. **Zero overhead on non-targeted syscalls** — same as existing fault injection.

tc/netem can be added later (Step 4b) if packet-level control is needed.

### Expected Outcome

Extended fault rule syntax with `delay` action:

```bash
# Delay every connect() by 200ms
faultbox run --fault "connect=delay:200ms:100%" ./my-service

# Reject 30% of connections, delay the rest by 50ms
faultbox run \
  --fault "connect=ECONNREFUSED:30%" \
  --fault "connect=delay:50ms:70%" \
  ./my-service

# Simulate slow writes (100ms per write call)
faultbox run --fault "write=delay:100ms:100%" ./my-service

# Combination: slow network + occasional disk errors
faultbox run \
  --fault "connect=delay:200ms:100%" \
  --fault "sendto=delay:50ms:20%" \
  --fault "write=EIO:5%:/data/*" \
  ./my-service
```

Example output:

```
INF [engine] syscall intercepted name=connect pid=1234 decision=delay(200ms) dst=10.0.0.5:5432
INF [engine] syscall intercepted name=connect pid=1234 decision=deny(ECONNREFUSED)
INF [engine] syscall intercepted name=sendto pid=1234 decision=delay(50ms)
```

### What We Learn

- **Delay fidelity:** Does sleeping in the notification loop produce accurate
  delays? The overhead per syscall (poll + ioctl) adds ~0.1-1ms; is this
  acceptable on top of the requested delay?
- **Interaction with Go runtime:** Go's net poller uses non-blocking sockets +
  `epoll`. When we delay `connect()`, does the runtime handle the delayed return
  correctly, or does it spin on EINPROGRESS?
- **Timeout simulation accuracy:** Delaying `connect()` for 30s should trigger
  the application's connect timeout. Does this actually work, or does the OS
  timeout first?

### Technical Approach

#### Extended Fault Rule Actions

Current rule format: `SYSCALL=ERRNO:PROBABILITY%[:PATH_GLOB]`

New: the ERRNO position can also be a delay:

```
SYSCALL=delay:DURATION:PROBABILITY%
```

Where `DURATION` is a Go duration string (`100ms`, `1s`, `500us`).

The `FaultRule` struct gets an `Action` field:

```go
type FaultAction int
const (
    ActionDeny  FaultAction = iota  // Return errno (existing behavior)
    ActionDelay                      // Sleep then allow
)

type FaultRule struct {
    Syscall     string
    Action      FaultAction
    Errno       syscall.Errno    // For ActionDeny
    Delay       time.Duration    // For ActionDelay
    Probability float64
    PathGlob    string
}
```

#### Notification Loop Changes

For `ActionDelay`, the loop does:

```go
case ActionDelay:
    time.Sleep(rule.Delay)
    seccomp.Allow(listenerFd, req.ID)
```

This is trivially simple — the kernel has already paused the target's syscall.
We just wait before telling the kernel to proceed.

**Concern:** sleeping in the notification loop blocks processing of other
intercepted syscalls. For the PoC (single-service), this is fine because the
target is single-threaded from the kernel's perspective (one syscall at a time
per thread). For multi-threaded targets, we may need to handle notifications
concurrently (spawn a goroutine per notification).

**Solution for multi-threaded targets:** Process each notification in its own
goroutine. The notification loop receives, spawns a handler goroutine, and
immediately goes back to polling:

```go
req, _ := seccomp.Receive(listenerFd)
go s.handleNotification(listenerFd, req, ruleMap)
```

This allows concurrent delay handling without blocking the loop.

#### Destination Filtering (Future)

For network syscalls, we could filter by destination address (like path filtering
for file syscalls). This is a future enhancement:

```
--fault "connect=ECONNREFUSED:100%:10.0.0.5:5432"
```

For the PoC, network faults apply to all calls of the targeted syscall.

### Implementation Plan

1. **Extend FaultRule with Action/Delay** — add `FaultAction` enum, `Delay`
   field, update parser to recognize `delay:DURATION` in the errno position.

2. **Update notification loop** — spawn goroutine per notification for concurrent
   handling. For `ActionDelay`, sleep then allow. For `ActionDeny`, deny
   immediately (existing).

3. **Log delay decisions** — `decision=delay(200ms)` in structured logs.

4. **Update target binary** — add a network call with configurable timeout so
   we can observe delay injection effects.

5. **Test** — verify delays are injected, test concurrent delays, measure
   accuracy.

### Non-Goals for Step 4

- No tc/netem or veth pair setup (requires host CAP_NET_ADMIN)
- No packet-level fault injection (loss, corruption, reordering) — **low priority
  for future steps too.** P-lang operates at message granularity, not packet level.
  Seccomp-based syscall faults cover the abstraction level that matters for
  formal verification. Packet-level faults are a "nice to have" for realism,
  not a prerequisite for the verification loop.
- No destination IP/port filtering on network syscalls (future)
- No bandwidth throttling
- No DNS fault injection

### Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Delay accuracy (OS scheduling jitter) | Medium | Low | Acceptable for PoC; measure and document |
| Go runtime EINPROGRESS spin on delayed connect | Low | Medium | Test with Go target; may need to delay at different syscall |
| Notification loop blocking on single-thread targets | Low | Low | Goroutine-per-notification handles this |
| seccomp notification timeout (kernel may kill long delays) | Low | High | Test with 30s+ delays; check kernel behavior |

---

## Findings

Tested on Lima VM (Ubuntu 24.04, kernel 6.8.0-101-generic, Go 1.26.1).

### What Worked

- **Delay injection is trivially simple:** `time.Sleep(rule.Delay)` before
  `seccomp.Allow()` — the kernel has already paused the target's syscall, we
  just wait before responding. No new kernel APIs or privileges needed.

- **Goroutine-per-notification:** Each seccomp notification is handled in its
  own goroutine via `sync.WaitGroup`. This prevents delay rules from blocking
  processing of other intercepted syscalls. Multi-threaded targets work correctly
  (observed different PIDs for concurrent goroutines in the target).

- **Delay accuracy:** 500ms requested delay consistently produces ~500ms actual
  delay per syscall (measured via target timing output). OS scheduling jitter
  adds <1ms — negligible for fault injection purposes.

- **Composes with deny rules:** `--fault "openat=delay:100ms:100%:/tmp/*"` and
  `--fault "write=EIO:100%"` work together in the same session without conflict.

- **Parser extension:** `delay:DURATION` in the errno position is unambiguous —
  no errno name starts with "delay". The parser cleanly routes to `parseDelayRule`
  vs `parseDenyRule`.

### Architecture Delivered

```
internal/engine/fault.go         → FaultAction enum (ActionDeny, ActionDelay)
                                    Extended parser: "connect=delay:200ms:100%"
internal/engine/launch_linux.go  → Goroutine-per-notification loop
                                    handleNotification() with delay/deny switch
                                    logSyscall() helper for consistent logging
poc/target/main.go               → Timing output for observing delays
```

### Open Questions for Next Steps

- **Delay + path filtering for network syscalls:** Currently delay works on any
  syscall but has no destination filtering (e.g., "delay connect to 10.0.0.5 only").
  This would require reading sockaddr from `/proc/pid/mem` — similar to path
  reading for file syscalls. Deferred to Step 7 (message ordering).
- **Long delays and kernel timeout:** Tested with 10s and 30s delays — both work
  correctly. The kernel does NOT impose a timeout or SIGKILL on seccomp-notified
  syscalls waiting for a supervisor response. The target's thread is simply paused
  until we respond. This means we can simulate arbitrarily long timeouts (e.g.,
  30s connect timeout) without any special handling. Confirmed on kernel 6.8.

---

## Connection to Roadmap

```
Step 4 (this) → delay + reject at syscall level (single service)
  ↓
Step 5 → FUSE filesystem faults (completes single-service primitives)
  ↓
Step 6 → architecture review: multi-service topology
  - Multiple targets, each isolated + faulted
  - veth pairs between namespaces (may require helper with CAP_NET_ADMIN)
  - tc/netem on veth for packet-level faults
  ↓
Step 7 → network message ordering (P-spec replay)
  - Seccomp delay on send/recv to reorder messages between actors
  - This step's delay mechanism is the foundation
```
