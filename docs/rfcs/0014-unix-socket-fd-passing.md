# RFC-014: Unix Socket FD Passing for Container Seccomp

- **Status:** Implemented (v0.6.0)
- **Author:** Boris Glebov, Claude Opus 4.6
- **Created:** 2026-04-16
- **Branch:** `rfc/014-unix-socket-fd`
- **Addresses:** FB-003, FB-007

## Summary

Replace `pidfd_getfd` with Unix domain socket SCM_RIGHTS for passing the
seccomp listener fd from the container shim to the host faultbox process.
This eliminates PID tracking entirely, enabling fault injection on
containers with multi-process entrypoints (Java/shell/supervisor).

## Motivation

### The problem

Faultbox's container mode uses a two-step process:

1. **Shim** (inside container): installs seccomp filter, writes listener fd
   number to a file
2. **Host** (faultbox): reads fd number, calls `pidfd_getfd(container_PID, fd)`
   to copy the fd into its own process

This fails when the container entrypoint forks:

```
shim → exec → /bin/bash (Confluent entrypoint)
                  ├─ configures Kafka
                  ├─ fork() → java kafka.Kafka   ← new PID
                  └─ exits                        ← original PID gone

Host: pidfd_getfd(original_PID, 7) → FAILS (PID dead)
```

**Affected containers:** Confluent Kafka, Confluent Zookeeper, Cassandra,
Elasticsearch, any Java service with a shell wrapper, any container using
supervisord, tini with shell entrypoints, or multi-stage init scripts.

### Current workaround (v0.5.0)

When `pidfd_getfd` fails, faultbox falls back to no-seccomp mode: the
container runs without fault injection. This unblocks the topology but
means **fault rules are silently skipped** on these containers.

### Why pidfd_getfd is fundamentally wrong for this

`pidfd_getfd` requires:
- Knowing the exact PID of the process holding the fd
- `CAP_SYS_PTRACE` or root on the host
- The target process to be alive at the moment of the call

All three assumptions break with fork-based entrypoints. The fd survives
(seccomp listener refcount keeps it alive in the child), but the PID
we tracked is gone.

## Design

### Core change: Unix socket fd passing via SCM_RIGHTS

Replace the file-based fd reporting + `pidfd_getfd` copy with a Unix
domain socket and `SCM_RIGHTS` ancillary data:

```
Host (faultbox)                     Container (shim)
───────────────                     ────────────────
creates socketDir/fd.sock
listens on Unix socket
                                    shim installs seccomp filter (fd=7)
                                    shim connects to Unix socket
                                    shim sends fd=7 via SCM_RIGHTS ──►
receives fd on host side
sends ACK byte ────────────────────►
                                    shim reads ACK
                                    shim exec's original entrypoint
                                        └─ fork, shell, Java — doesn't matter
                                           seccomp filter is inherited
```

**Key insight:** the fd transfer happens BEFORE the exec. The shim
connects, sends the fd, waits for ACK, then exec's. At that point, the
host already has a copy of the listener fd in its own process. Whatever
the entrypoint does after (fork, exec, exit) doesn't affect the host's fd.

### Why this solves everything

| Problem | pidfd_getfd | Unix socket |
|---------|------------|-------------|
| Fork-based entrypoint | Fails (PID dies) | Works (fd sent before exec) |
| Multi-process init | Fails (wrong PID) | Works (fd sent before exec) |
| CAP_SYS_PTRACE required | Yes | No |
| Root required | Yes (or capability) | No |
| PID namespace crossing | Complex | Not needed |
| Timing-dependent | Yes (race between fork and pidfd call) | No (synchronous handshake) |

### Implementation

#### Shim changes (`cmd/faultbox-shim/main.go`)

Replace the file-write + busy-wait pattern with a Unix socket send:

```go
// Current (file-based):
reportFile.WriteString(strconv.Itoa(listenerFd))  // write fd number
reportFile.Close()
for { os.Stat(cfg.AckPath) }  // busy-wait for ACK file

// New (socket-based):
conn, _ := net.Dial("unix", cfg.SocketPath)       // connect to host
rights := unix.UnixRights(listenerFd)              // serialize fd
conn.WriteMsgUnix([]byte("fd"), rights, nil)       // send fd via SCM_RIGHTS
buf := make([]byte, 1)
conn.Read(buf)                                     // wait for ACK byte
conn.Close()
```

**ShimConfig changes:**

```go
type ShimConfig struct {
    SyscallNrs []uint32 `json:"syscall_nrs"`
    Entrypoint []string `json:"entrypoint"`
    Cmd        []string `json:"cmd"`
    SocketPath string   `json:"socket_path"` // NEW: replaces report_path + ack_path
}
```

#### Host changes (`internal/container/fd_linux.go`)

Replace `pidfd_getfd` with a Unix socket listener:

```go
// Current:
func waitForListenerFd(ctx context.Context, reportPath string, hostPID int) (int, error) {
    // poll file for fd number
    // pidfd_open(hostPID)
    // pidfd_getfd(pidfd, childFd)
}

// New:
func waitForListenerFd(ctx context.Context, socketPath string) (int, error) {
    listener, _ := net.Listen("unix", socketPath)
    defer listener.Close()

    conn, _ := listener.Accept()  // blocks until shim connects
    defer conn.Close()

    // Receive fd via SCM_RIGHTS
    buf := make([]byte, 4)
    oob := make([]byte, unix.CmsgLen(4))
    _, oobn, _, _, _ := conn.(*net.UnixConn).ReadMsgUnix(buf, oob)

    scms, _ := unix.ParseSocketControlMessage(oob[:oobn])
    fds, _ := unix.ParseUnixRights(&scms[0])
    fd := fds[0]

    // Send ACK
    conn.Write([]byte("k"))

    return fd, nil
}
```

#### Launch changes (`internal/container/launch.go`)

```go
// Current:
shimCfg := ShimConfig{
    SyscallNrs: cfg.SyscallNrs,
    Entrypoint: origEntrypoint,
    Cmd:        origCmd,
    ReportPath: "/var/run/faultbox/listener-fd",
    AckPath:    "/var/run/faultbox/ack",
}

// Start container, then:
listenerFd, err := waitForListenerFd(ctx, reportPath, hostPID)
os.WriteFile(ackPath, []byte("ok"), 0644)

// New:
shimCfg := ShimConfig{
    SyscallNrs: cfg.SyscallNrs,
    Entrypoint: origEntrypoint,
    Cmd:        origCmd,
    SocketPath: "/var/run/faultbox/fd.sock",
}

// Start listener BEFORE starting container:
go func() {
    listenerFd, err = waitForListenerFd(ctx, socketDir+"/fd.sock")
}()

// Start container (shim connects to socket, sends fd, waits for ACK)
// waitForListenerFd returns with the fd
```

**Host PID is no longer needed.** The `hostPID` variable, the retry loop
to get it, and the `ContainerPID` call are all removed.

### Bind mount

The socket directory is already bind-mounted:

```go
// Current:
binds := []string{
    cfg.ShimPath + ":/faultbox-shim:ro",
    socketDir + ":/var/run/faultbox:rw",
}

// Same — no change needed. The socket file lives in the same directory.
```

### Backward compatibility

The shim binary must match the host binary. Since both are built together
(`make demo-build`), this is already the case. The old file-based protocol
is removed entirely — no fallback needed.

### What about the no-seccomp fallback?

The v0.5.0 fallback (detect failure, relaunch without shim) remains for
cases where the seccomp filter itself can't be installed (e.g., kernel
too old, seccomp disabled). But the **fd passing failure** path is
eliminated — Unix sockets don't have the PID-tracking failure mode.

## Impact

- **Breaking changes:** Shim protocol changes. Both `faultbox` and
  `faultbox-shim` must be updated together (already required).
- **Removes:** `pidfd_getfd`, `pidfd_open`, `CAP_SYS_PTRACE` requirement,
  `ContainerPID` call, PID retry loop, file-based reporting, ACK file
  busy-wait.
- **Adds:** Unix socket listener (host), Unix socket connect + SCM_RIGHTS
  send (shim).
- **Performance:** Faster — direct socket handshake vs file polling +
  pidfd syscalls. Eliminates the 50ms poll loop.
- **Security:** Better — no `CAP_SYS_PTRACE`, no root required for fd
  acquisition.

## Open Questions

1. **Socket path length limit.** Unix socket paths are limited to 108
   bytes. The current `socketDir` is `/tmp/faultbox-sockets/<service-name>/`.
   Long service names could exceed the limit.
   Proposed: use a short hash if the path would be too long.

2. **Timeout.** If the container fails to start (image pull error, crash
   on init), the socket Accept blocks forever. Need a context timeout
   (already exists via `ctx`).

3. **Multiple seccomp filters.** If the entrypoint installs its own
   seccomp filter before ours (rare), the listener fd might be different.
   Not a new problem — same issue with `pidfd_getfd`.

## Implementation Plan

### Step 1: Update shim to use Unix socket

- Replace file write + busy-wait with `net.Dial("unix")` + `SCM_RIGHTS`
- Update `ShimConfig`: remove `ReportPath`/`AckPath`, add `SocketPath`
- Test: shim sends fd, receives ACK, exits cleanly

### Step 2: Update host to use Unix socket listener

- Replace `waitForListenerFd` to use `net.Listen("unix")` + `Accept` +
  `ReadMsgUnix` + `ParseUnixRights`
- Remove `hostPID` tracking, `ContainerPID` call, PID retry loop
- Remove `pidfd_open`/`pidfd_getfd` imports
- Test: host receives fd, sends ACK, uses fd for seccomp notification

### Step 3: Update launch.go

- Start socket listener before container start
- Pass `SocketPath` in `ShimConfig` instead of `ReportPath`/`AckPath`
- Remove `hostPID` from `LaunchResult` (no longer needed)
- Test: full container launch + fault injection on single-process container

### Step 4: Test with multi-process containers

- Test with `confluentinc/cp-kafka:7.6` (shell → fork → Java)
- Test with `confluentinc/cp-zookeeper:7.6`
- Test with `cassandra:4.1`
- Verify fault injection works on the forked Java process
- Test: fault(kafka, write=deny("EIO")) actually fires

### Step 5: Remove fallback (optional)

- The v0.5.0 no-seccomp fallback becomes a safety net for kernel issues
  only, not for fork-based entrypoints
- Update warning message: "seccomp not supported on this kernel" instead
  of "multi-process entrypoint"

## References

- [seccomp(2) — SECCOMP_FILTER_FLAG_NEW_LISTENER](https://man7.org/linux/man-pages/man2/seccomp.2.html)
- [unix(7) — SCM_RIGHTS](https://man7.org/linux/man-pages/man7/unix.7.html)
- [pidfd_getfd(2)](https://man7.org/linux/man-pages/man2/pidfd_getfd.2.html) — what we're replacing
- Current shim: `cmd/faultbox-shim/main.go`
- Current fd acquisition: `internal/container/fd_linux.go`
- Current launch: `internal/container/launch.go`
- FB-003: pidfd_getfd fails on multi-process containers
- FB-007: Support multi-process containers (Java ecosystem)
