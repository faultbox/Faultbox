# PoC Step 2: Syscall Interception via seccomp-notify

**Branch:** `poc/step-2-syscall-intercept`
**Status:** In Progress
**Date:** 2026-03-23

## Executive Summary

### What

Add a syscall interception layer to the launcher from Step 1. Using Linux's
seccomp user notifications (`SECCOMP_RET_USER_NOTIF`), Faultbox will be able
to intercept specific syscalls from the target process, inspect their arguments,
and decide what happens: allow, deny with an error code, or inject a delay.

### Why This Is Step 2

Step 1 gave us an isolated process. Now we need to **observe and control** what
that process does. Syscall interception is the lowest-level control point — every
meaningful action a program takes (open a file, connect to a network, allocate
memory, read the clock) passes through a syscall. Controlling syscalls means
controlling reality for the target process.

| What becomes possible | How |
|---|---|
| **Filesystem faults** (Step 3) | Intercept `open`, `read`, `write`, `fsync` → return `EIO`, `ENOSPC` |
| **Network faults** (Step 4) | Intercept `connect`, `sendto`, `recvfrom` → return `ECONNREFUSED`, inject delay |
| **Clock control** (future) | Intercept `clock_gettime` → return fake time |
| **Deterministic I/O** (future) | Log every syscall with args → replay exact sequence |
| **Fault scenarios** | "Make the 3rd write to `/data/wal` fail with `EIO`" — surgical precision |

Without syscall interception, we can only inject faults at coarse granularity
(whole network down, whole filesystem broken). With it, we get **per-operation
control** — the foundation of intelligent fault injection.

### Why seccomp-notify over alternatives

| Approach | Overhead | Control | Why not |
|---|---|---|---|
| **ptrace** | High (2 context switches/syscall) | Full | Too slow for production-like testing |
| **eBPF (bpf_override_return)** | Low | Return value only | Cannot inspect args, requires `CONFIG_BPF_KPROBE_OVERRIDE` |
| **LD_PRELOAD** | Low | Userspace only | Doesn't work with static Go binaries |
| **seccomp-notify** | Low (only targeted syscalls) | Full args + return | Best balance of performance and control |

seccomp-notify works by installing a BPF filter that flags specific syscalls.
When the target hits one, the kernel pauses the target and sends a notification
to the supervisor (Faultbox). The supervisor inspects the syscall, decides the
outcome, and responds. Only targeted syscalls incur overhead — everything else
runs at full speed.

### Expected Outcome

A working `faultbox run --fault "open=ENOENT:50%" <binary>` that:

1. **Installs a seccomp filter** on the target process before it starts
2. **Receives notifications** when the target makes intercepted syscalls
3. **Logs every intercepted syscall** with structured details (name, args, decision)
4. **Can inject failures** — return specified errno at specified probability
5. **Passes through by default** — unmatched syscalls and non-faulted calls proceed normally

Example:

```bash
# 50% of open() calls fail with ENOENT
faultbox run --fault "open=ENOENT:50%" ./poc/target/main

# Expected output:
# INF [engine] namespace enabled type=PID
# INF [engine] namespace enabled type=NET
# INF [engine] seccomp filter installed syscalls=["open","openat"]
# INF [engine] target started pid=1
# === Faultbox PoC Target ===
# PID: 1
# INF [syscall] intercepted name=openat path=/tmp/faultbox-target-test decision=allow
# FS: wrote and read 14 bytes
# INF [syscall] intercepted name=openat path=/tmp/faultbox-target-test decision=deny errno=ENOENT
# fs write failed: open /tmp/faultbox-target-test: no such file or directory
# INF [engine] session completed exit_code=1
```

### What We Learn

- **seccomp-notify ergonomics in Go:** Is `libseccomp-golang` sufficient or do we
  need raw syscalls? How does the notification fd get passed to the supervisor?
- **Filter installation timing:** The BPF filter must be installed after fork but
  before exec. How does this work with Go's `exec.Cmd`? May need `SysProcAttr.Pdeathsig`
  or a small init process inside the namespace.
- **Performance baseline:** What's the overhead per intercepted syscall? Is it
  fast enough that we can intercept `read`/`write` without distorting behavior?
- **TOCTOU safety:** seccomp-notify has a known TOCTOU issue with pointer args
  (the target can modify memory between inspection and execution). How do we
  handle this for `open(path, ...)`?
- **Interaction with USER namespace:** Does seccomp-notify work correctly when
  combined with `CLONE_NEWUSER`? There are known edge cases.

### Technical Approach

```
┌─────────────────────────────────────┐
│  Faultbox (supervisor)              │
│                                     │
│  1. Create namespaces (Step 1)      │
│  2. Install seccomp filter          │
│  3. Start target                    │
│  4. Loop:                           │
│     - Receive notification (fd)     │
│     - Inspect syscall + args        │
│     - Apply fault rules             │
│     - Respond (allow / deny+errno)  │
│  5. Target exits → session done     │
└───────────────┬─────────────────────┘
                │ seccomp notify fd
                │
┌───────────────▼─────────────────────┐
│  Target process (isolated)          │
│                                     │
│  open("/tmp/file", O_RDWR)          │
│    → kernel pauses target           │
│    → sends notification to Faultbox │
│    → Faultbox responds: ALLOW       │
│    → kernel resumes target          │
│    → open() returns fd              │
└─────────────────────────────────────┘
```

Key implementation pieces:
- **Seccomp BPF filter:** Built with `libseccomp-golang`, targets specific syscall numbers
- **Notification loop:** Goroutine reading from the notify fd, dispatching to fault rules
- **Fault rule engine:** Parse `--fault "syscall=errno:probability"` into rules
- **Response handler:** Either `SECCOMP_USER_NOTIF_FLAG_CONTINUE` (allow) or set errno

### Non-Goals for Step 2

- No complex fault scenarios (sequences, conditional faults) — just per-syscall rules
- No persistent fault state (counters, "fail after N calls") — just probability
- No UI or API for dynamic rule changes — static rules from CLI flags
- No TOCTOU mitigation for pointer args — log the limitation, address later

### Risk: seccomp-notify + exec.Cmd

The main technical risk is filter installation. seccomp filters are inherited
across `fork()` but the filter must be installed by the target process itself
(or its parent before fork). With `exec.Cmd`, we don't control the code between
fork and exec.

**Possible solutions:**
1. Use `SysProcAttr.AmbientCaps` and have the target install its own filter (invasive)
2. Write a small C/Go "init" shim that installs the filter then execs the real target
3. Use `Ctty`/`Setpgid` hooks in `SysProcAttr` — limited
4. Explore `SECCOMP_FILTER_FLAG_NEW_LISTENER` with pidfd — the supervisor installs
   the filter and gets the notify fd back

Option 4 is cleanest if supported on kernel 6.8. This is the first thing to spike.
