# PoC Step 3: Unified Launch — Namespaces + Seccomp + Path Filtering

**Branch:** `poc/step-3-unified-launch`
**Status:** Complete
**Date:** 2026-03-24

## Executive Summary

### What

Combine the two launch paths from Steps 1 and 2 into a single unified path.
Today, `--fault` disables namespace isolation — you get either sandboxing OR
fault injection. Step 3 makes them work together: the target runs in isolated
namespaces AND has a seccomp filter attached.

Additionally, add **path-based filtering** for file syscalls (`openat`, etc.)
so that faults can target specific paths while leaving system paths (`/lib`,
`/usr/lib`) untouched — solving the dynamic linker problem from Step 2.

### Why This Is Step 3

This is the **unification step** — without it, Steps 4 and 5 can't build on
a solid foundation:

| Dependency | Why |
|---|---|
| **Step 4 (Network faults)** | Needs NET namespace to apply tc/netem rules to the target only |
| **Step 5 (FUSE filesystem)** | Needs MNT namespace to mount a FUSE overlay without affecting host |
| **Step 6 (Architecture review)** | Multi-service scenarios need each service fully isolated + faulted |
| **Path filtering** | 100% openat fault kills the dynamic linker (Step 2 limitation) — must be solved before filesystem faults are useful |

The current two-path split (exec.Cmd with clone flags vs. ForkExec shim) is
also a maintenance burden. A single launch path simplifies everything downstream.

### Expected Outcome

A working `faultbox run --fault "openat=ENOENT:100%:/data/*" ./my-service` that:

1. **Launches the target in isolated namespaces** (PID, NET, MNT, USER) — same as Step 1
2. **Installs a seccomp filter** for fault injection — same as Step 2
3. **Both at the same time** — the shim sets up namespaces before exec
4. **Supports path globs** on file syscalls — `openat=ENOENT:100%:/data/*` only faults opens under `/data/`
5. **Passes ENV variables** to the target — `--env KEY=VALUE` flag
6. **Defaults safely** — without path filter, file faults exclude `/lib`, `/usr/lib`, `/proc`, `/sys`, `/dev` (system paths)

Example:

```bash
# Isolated + faulted: target in sandbox with 100% openat failures on /data/
faultbox run \
  --fault "openat=ENOENT:100%:/data/*" \
  --env DB_PATH=/data/mydb \
  ./my-service

# Expected output:
# INF [engine] namespace enabled type=PID
# INF [engine] namespace enabled type=NET
# INF [engine] namespace enabled type=MNT
# INF [engine] namespace enabled type=USER
# INF [engine] seccomp filter installed syscalls=["openat"]
# INF [engine] target started pid=1 listener_fd=5
# INF [engine] syscall intercepted name=openat path=/usr/lib/libc.so.6 decision=allow (system path)
# INF [engine] syscall intercepted name=openat path=/data/mydb/wal decision=deny(ENOENT)
# === Target ===
# PID: 1
# DB open failed: no such file or directory
# INF [engine] session completed exit_code=1
```

### What We Learn

- **Namespace + seccomp composition:** Can the shim set clone flags AND install
  a seccomp filter? `CLONE_NEWUSER` + `seccomp(SET_MODE_FILTER)` may have edge
  cases with capability checks.
- **Two-stage ForkExec:** The shim currently uses `syscall.ForkExec`. To add
  clone flags, we need `SysProcAttr.Cloneflags` — which means switching to
  `exec.Cmd.Start()` or raw `clone()` + `exec()`. Key design decision.
- **Path filtering overhead:** Reading `/proc/pid/mem` for every `openat` adds
  latency. Is it acceptable? Could we cache the dynamic linker's fd range?
- **System path exclusion UX:** Should the default exclusion list be hardcoded
  or configurable? For the PoC, hardcoded is fine.

### Technical Approach

#### Unified Launch Path

Replace the current two paths with a single shim-based launch:

```
Parent (faultbox)
  │
  ├─ Always uses shim path (even without --fault, for consistency)
  ├─ ForkExec(self) with clone flags + _FAULTBOX_SECCOMP_CHILD env
  │
  └→ Child (faultbox shim)
       ├─ Already in new namespaces (PID, NET, MNT, USER)
       ├─ runtime.LockOSThread()
       ├─ prctl(PR_SET_NO_NEW_PRIVS)
       ├─ If faults: seccomp(SET_MODE_FILTER) → listener fd
       ├─ Write listener fd (or "no-filter" signal) to pipe
       ├─ unix.Exec(targetBinary) → filter survives exec
       │
  Parent reads pipe, optionally runs notification loop
```

Key change: `syscall.ForkExec` → `exec.Cmd.Start()` with `SysProcAttr` that
sets both `Cloneflags` and inherits the shim env var. The child is now born
into namespaces AND can install a seccomp filter.

#### Path-Based Fault Filtering

Extended fault rule syntax:

```
--fault "SYSCALL=ERRNO:PROBABILITY%[:PATH_GLOB]"
```

- Path glob is optional — omitting it means "match all" (with system path exclusion)
- Path glob uses `filepath.Match` semantics: `*` matches within a segment,
  `**` is not supported (keep it simple)
- System path exclusion list (hardcoded for PoC):
  - `/lib/*`, `/usr/lib/*`, `/usr/lib64/*` — shared libraries
  - `/proc/*`, `/sys/*`, `/dev/*` — virtual filesystems
  - `/etc/ld.so.*` — dynamic linker config

Path filtering happens in the notification loop, not in the BPF filter (BPF
can't inspect pointer args). Flow:

```
1. BPF filter triggers USER_NOTIF for openat
2. Notification loop receives it
3. Read path from /proc/pid/mem (already implemented)
4. Check path against:
   a. System exclusion list → allow if matched
   b. Rule path glob → deny if matched
   c. Default → allow
```

#### ENV Variable Passthrough

Simple `--env KEY=VALUE` flag, passed to the target process:

- Added to `SessionConfig` as `Env []string`
- Shim passes them through to `unix.Exec` environment
- Multiple `--env` flags supported

### Implementation Plan

1. **Unify the shim** — modify `StartWithFilter` to accept clone flags and
   uid/gid mappings. The parent uses `exec.Cmd` with `SysProcAttr.Cloneflags`
   to ForkExec the shim child into namespaces.

2. **Path filtering in fault rules** — extend `FaultRule` with optional
   `PathGlob string`. Extend `ParseFaultRule` to handle the 4th segment.
   Add system path exclusion logic in the notification loop.

3. **ENV passthrough** — add `--env` flag to CLI, pass through shim config
   to target exec.

4. **Remove dual-path** — session.go always uses the shim path. Without
   `--fault`, the shim just sets up namespaces (no seccomp filter) and execs
   the target. The notification loop is skipped.

5. **Test** — verify namespaces + faults work together, path filtering
   excludes system paths, ENV vars reach the target.

### Non-Goals for Step 3

- No FUSE filesystem (Step 5)
- No network fault injection via tc/netem (Step 4)
- No `**` recursive glob patterns — `*` within a single path segment is sufficient
- No configurable system path exclusion — hardcoded list for PoC
- No working directory (`--workdir`) flag — add later if needed

### Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `CLONE_NEWUSER` + seccomp capability conflict | Medium | High | Test early; fall back to installing filter before entering userns |
| `exec.Cmd` doesn't support shim pattern | Low | High | Fall back to raw `clone()` via `unix.Syscall` |
| Path reading overhead on high-frequency openat | Low | Medium | Acceptable for PoC; cache/optimize later |

---

## Findings

Tested on Lima VM (Ubuntu 24.04, kernel 6.8.0-101-generic, Go 1.26.1).

### What Worked

- **`syscall.ForkExec` with `Cloneflags`:** The simplest approach won. Adding
  `SysProcAttr{Cloneflags: CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWNS|CLONE_NEWUSER}`
  to the existing `ForkExec` call creates the child directly in namespaces.
  No need for `exec.Cmd` or raw `clone()`. The child then installs the seccomp
  filter and execs the target — both mechanisms compose cleanly.

- **No capability conflicts:** `CLONE_NEWUSER` + `seccomp(SET_MODE_FILTER)` works
  without issues. The user namespace provides the capabilities needed for seccomp,
  and `PR_SET_NO_NEW_PRIVS` is set inside the namespace.

- **System path exclusion:** Reading paths from `/proc/pid/mem` and checking against
  a prefix list (`/lib/`, `/usr/lib/`, `/proc/`, `/sys/`, `/dev/`, `/etc/ld.so.*`)
  reliably protects the dynamic linker. 100% `openat` fault now works — the target
  starts, loads shared libraries, and only application file opens are faulted.

- **Path glob filtering:** `filepath.Match` provides sufficient glob semantics for
  the PoC. `--fault "openat=ENOENT:100%:/data/*"` correctly targets only matching
  paths while allowing all others through without system path exclusion overhead.

- **Unified shim removes dual-path maintenance:** The old split (exec.Cmd for
  namespaces vs ForkExec for seccomp) is gone. A single `Launch()` function handles
  all combinations: namespaces only, seccomp only, or both together.

### Architecture Delivered

```
cmd/faultbox/main.go                  → CLI with --fault, --env, --debug flags
internal/seccomp/launch.go            → Portable LaunchConfig + IDMapping types
internal/seccomp/shim_linux.go        → Unified Launch() + RunShimChild()
internal/seccomp/filter_linux.go      → BPF filter builder (unchanged)
internal/engine/launch_linux.go       → Session launch + notification loop with path filtering
internal/engine/launch_other.go       → Non-Linux stub
internal/engine/session.go            → Session lifecycle (simplified, single path)
internal/engine/fault.go              → Fault rules with PathGlob + system path exclusion
```

Removed: `intercept_linux.go`, `intercept_other.go`, `namespace_linux.go`, `namespace_other.go`

### Open Questions for Next Steps

- **Network faults via tc/netem:** NET namespace is now always active. Step 4 needs
  to set up a veth pair inside it and apply netem rules for latency/loss injection.
- **FUSE in user namespace:** MNT namespace is active. Step 5 needs to verify that
  FUSE mounts work inside `CLONE_NEWUSER` without root.
- **Path glob depth:** `filepath.Match` doesn't support `**` for recursive matching.
  `/data/*` matches `/data/foo` but not `/data/foo/bar`. For the PoC this is fine;
  may need `doublestar` or custom glob for deeper hierarchies.

---

## Connection to Roadmap

```
Step 3 (this) → unified isolated + faulted process
  ↓
Step 4 → add tc/netem inside NET namespace (network faults)
Step 5 → mount FUSE inside MNT namespace (filesystem faults)
  ↓
Step 6 → architecture review: multi-service topology
Step 7 → network message ordering between services (P-spec replay)
  ↓
Steps 8-10 → formal modeling integration
```
