# PoC Step 2: Syscall Interception via seccomp-notify

**Branch:** `poc/step-2-syscall-intercept`
**Status:** Complete
**Date:** 2026-03-24

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
- **Seccomp BPF filter:** Pure Go, hand-built BPF program (no libseccomp cgo dependency)
- **Notification loop:** Goroutine reading from the notify fd, dispatching to fault rules
- **Fault rule engine:** Parse `--fault "syscall=errno:probability"` into rules
- **Response handler:** Either `SECCOMP_USER_NOTIF_FLAG_CONTINUE` (allow) or set errno

### Non-Goals for Step 2

- No complex fault scenarios (sequences, conditional faults) — just per-syscall rules
- No persistent fault state (counters, "fail after N calls") — just probability
- No UI or API for dynamic rule changes — static rules from CLI flags
- No TOCTOU mitigation for pointer args — log the limitation, address later

---

## Findings

Tested on Lima VM (Ubuntu 24.04, kernel 6.8.0-101-generic, Go 1.26.1).

### Risk Resolution: Filter Installation

The main risk was: how to install a seccomp filter between fork() and exec()
when Go's `exec.Cmd` gives no hook there?

**Solution: Re-exec shim pattern** (inspired by subtrace and runc).

```
Parent (faultbox)
  │
  ├─ creates pipe for fd communication
  ├─ ForkExec(self) with _FAULTBOX_SECCOMP_CHILD env
  │
  └→ Child (faultbox shim)
       ├─ runtime.LockOSThread()
       ├─ prctl(PR_SET_NO_NEW_PRIVS)
       ├─ seccomp(SET_MODE_FILTER, FLAG_NEW_LISTENER) → gets listener fd
       ├─ writes listener fd number to pipe → parent
       ├─ unix.Exec(targetBinary) → replaces self, filter survives exec()
       │
  Parent reads pipe, uses pidfd_open + pidfd_getfd to copy listener fd
  Parent now owns the seccomp listener fd and runs the notification loop
```

This is clean, requires no cgo, and works with any target binary (static or dynamic).

### What Worked

- **Pure Go seccomp:** No libseccomp-golang dependency needed. Hand-built BPF
  filter via `unix.Syscall(SYS_SECCOMP, ...)` with `SECCOMP_FILTER_FLAG_NEW_LISTENER`.
  The BPF program is simple: check arch → load syscall nr → match → RET_USER_NOTIF.

- **WAIT_KILLABLE_RECV (kernel 5.19+):** Critical for Go targets. Without it,
  Go's SIGURG (goroutine preemption) causes the seccomp-notified syscall to get
  interrupted and retried in a spin loop. Filter falls back gracefully on older kernels.

- **pidfd_getfd():** Cleanly transfers the listener fd from child to parent
  without SCM_RIGHTS/Unix socket complexity. Requires kernel 5.6+.

- **Write-fd exception in BPF filter:** The child must `write()` the listener fd
  number to the parent pipe before exec. If `write` is in the fault list, the BPF
  filter includes a special case: allow `write(fd=pipeFd)`, notify all other writes.
  This prevents deadlock.

- **Fault injection works:** `--fault "openat=ENOENT:100%"` correctly causes all
  file opens in the target to fail with ENOENT. `--fault "write=EIO:50%"` causes
  probabilistic write failures.

### Architecture Delivered

```
cmd/faultbox/main.go                → Re-exec shim entry point + CLI
internal/engine/fault.go             → Fault rule parser + errno map
internal/engine/fault_test.go        → Unit tests for rule parsing
internal/engine/intercept_linux.go   → Notification loop + fault matching
internal/engine/intercept_other.go   → Non-Linux stub
internal/engine/session.go           → Session routes to seccomp or namespace path
internal/seccomp/filter_linux.go     → BPF filter builder + notification I/O
internal/seccomp/shim_linux.go       → Re-exec shim child logic
internal/seccomp/arch_arm64.go       → ARM64 syscall table + arch constant
internal/seccomp/arch_amd64.go       → AMD64 syscall table + arch constant
internal/seccomp/seccomp_other.go    → Non-Linux stubs
```

### Open Questions for Next Steps

- **Namespaces + seccomp together:** Currently fault injection disables namespaces
  because the shim re-exec path doesn't set clone flags. Combining them requires
  the shim to also set up namespaces, or using a two-stage launch.
- **TOCTOU for path args:** `ReadStringFromProcess()` reads `/proc/pid/mem` to
  inspect `openat()` path argument, but the target could modify it between
  inspection and kernel execution. For logging this is fine; for security-critical
  decisions it needs `SECCOMP_IOCTL_NOTIF_ADDFD` or similar.
- **Performance:** Not yet benchmarked. Each intercepted syscall requires
  poll → ioctl(RECV) → decision → ioctl(SEND). Should measure overhead for
  high-frequency syscalls like `read`/`write`.
- **Dynamic linker interaction:** 100% `openat` fault blocks the dynamic linker
  from loading `libc.so.6` (exit 127). This is correct — the filter is process-wide
  including pre-main execution. Future: add path-based filtering to exclude
  `/lib`, `/usr/lib` from faults, or support static binaries only for full-openat faults.

### Connection to P-lang Verification

This is the bridge between formal verification and real fault injection:

```
P spec:  "if write to WAL fails after commit ack → data loss"
           ↓ P model checker generates trace
Faultbox: --fault "write=EIO" on specific fd/path (future: conditional rules)
           ↓ seccomp-notify intercepts exactly that write
Target:   experiences the precise failure scenario P predicted
```

"Partial determinism" — we don't need Antithesis-level full determinism.
We only control the **failure points** that P cares about, and let everything
else run naturally. Zero overhead on non-targeted syscalls.
