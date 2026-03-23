# PoC Step 1: Linux Launcher — Process Isolation with Namespaces

**Branch:** `poc/step-1-linux-launcher`
**Status:** Complete
**Date:** 2026-03-23

## Executive Summary

### What

Build a Go program (`faultbox run`) that launches an arbitrary Go binary inside
an isolated Linux environment using kernel namespaces. The launcher creates a
"sandbox" where the target process has its own PID tree, network stack, filesystem
mount table, and user identity — completely separated from the host.

### Why This Is Step 1

Every subsequent PoC step depends on having an isolated process:

| Future Step | Requires |
|---|---|
| Step 2: Syscall interception | PID namespace — to attach seccomp to the right process |
| Step 3: Filesystem faults | Mount namespace — to overlay a FUSE filesystem without affecting the host |
| Step 4: Network control | Network namespace — to apply tc/netem rules to the target only |
| Step 5: Scheduler control | Isolated PID — deterministic execution needs a controlled process tree |

Without isolation, every fault we inject would affect the host system. Namespaces
give us a "blast radius of one" — the target process sees a controlled world,
the host remains untouched.

### Expected Outcome

A working `faultbox run <binary> [args...]` command that:

1. **Creates Linux namespaces** (PID, NET, MNT, USER) for the target process
2. **Captures stdout/stderr** from the target and streams it to the caller
3. **Reports the exit code** faithfully (target exits 1 → faultbox exits 1)
4. **Runs headlessly** — no interactive input needed, works from `make env-exec`

Example:

```bash
# Build and run the target binary in an isolated sandbox
faultbox run ./poc/target/main

# Expected output:
# [faultbox] PID namespace: active (child PID 1)
# [faultbox] NET namespace: active (lo only)
# [faultbox] MNT namespace: active
# === Faultbox PoC Target ===
# PID: 1                          ← target sees itself as PID 1
# FS: wrote and read 14 bytes     ← filesystem works (private mount)
# NET: net failed: ...            ← no network (isolated netns, no veth yet)
# === Target done ===
# [faultbox] exit code: 0
```

### What We Learn

- Whether Go's `syscall.SysProcAttr` with `Cloneflags` is sufficient or if we
  need a helper binary (some namespace operations require `fork` before `exec`)
- How USER namespaces interact with capabilities (needed for Steps 2-4 which
  require `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`)
- The overhead of namespace creation (is it fast enough for interactive use?)
- Whether the target binary needs to be statically linked or if we can share
  libraries via bind mounts

### Technical Approach

Use Linux clone flags via Go's `exec.Cmd`:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: syscall.CLONE_NEWPID |  // own PID tree
                syscall.CLONE_NEWNET |  // own network stack
                syscall.CLONE_NEWNS  |  // own mount table
                syscall.CLONE_NEWUSER,  // own user mapping (unprivileged)
}
```

The USER namespace is key — it lets us create all other namespaces without root.
We map the current UID/GID to root inside the namespace, giving the target
process the capabilities it needs within its sandbox.

### Non-Goals for Step 1

- No syscall filtering (Step 2)
- No filesystem fault injection (Step 3)
- No network configuration (veth pairs, tc rules) — just isolated empty netns (Step 4)
- No CLI framework (cobra) yet — plain `os.Args` is fine for now

---

## Findings

Tested on Lima VM (Ubuntu 24.04, kernel 6.8.0-101-generic, Go 1.26.1).

### What Worked

- **Go's `SysProcAttr.Cloneflags`** is sufficient — no helper binary or fork needed.
  `exec.Cmd` with `CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWNS | CLONE_NEWUSER` just works.
- **USER namespace** allows unprivileged namespace creation. UID/GID mapping
  (`UidMappings`/`GidMappings`) maps host user to root inside the namespace.
- **Namespace creation overhead** is negligible — ~3ms total session time including
  process start, execution, and cleanup.
- **PID isolation** confirmed — target sees `PID: 1`.
- **Network isolation** confirmed — target gets empty netns (no interfaces),
  HTTP calls fail with "network is unreachable".
- **Mount isolation** confirmed — target can write to `/tmp` without affecting host.
- **Platform split** works cleanly — `namespace_linux.go` / `namespace_other.go`
  with build tags. macOS build compiles but returns a clear error if namespaces requested.

### Architecture Delivered

```
cmd/faultbox/main.go        → CLI frontend (thin, just parses args)
internal/engine/engine.go   → Core API: Engine.Run(), Engine.Status()
internal/engine/session.go  → Session lifecycle and state machine
internal/engine/namespace_linux.go  → Linux namespace setup
internal/engine/namespace_other.go  → Non-Linux stub
internal/logging/logging.go → slog-based logging (auto-detect TTY)
internal/logging/console.go → Colored console handler
```

### Open Questions for Next Steps

- **Static vs dynamic linking:** target binary worked as static Go binary.
  Need to test with CGO-enabled binaries that depend on shared libraries
  (may need bind mounts for `/lib` in the mount namespace).
- **procfs/sysfs:** Some programs expect `/proc` and `/sys`. Mount namespace
  gives us a private mount table but doesn't auto-mount these. May need to
  mount them in a future step.

---

## Ideas for Subsequent Steps

### Structured Logging System

We need a rich logging layer designed from the start — not bolted on later.
Faultbox produces complex, multi-layer events (namespace creation, syscall
interception, fault injection, target output) that must be understandable
both by humans debugging in a terminal and by machines ingesting into
observability platforms.

**Two output modes from the same event stream:**

| Mode | Format | Use Case |
|---|---|---|
| **Console** | Colored, human-readable, hierarchical | Developer running `faultbox run` in terminal |
| **Structured** | JSON lines (one object per event) | Piping to Kibana/Loki/jq, CI logs, agent consumption |

Selection via flag (`--log-format=console|json`) or auto-detect (TTY → console,
pipe → JSON).

**Why early:** If we build Step 1 with `fmt.Println`, we'll have to retrofit
every log site later. Designing the event model now means Steps 2-6 just emit
structured events and get both representations for free.

**Key design questions:**
- Event schema: what fields are always present? (timestamp, level, component,
  namespace, target_pid, phase)
- Nesting: how to represent parent/child relationships (faultbox → namespace →
  target → syscall)?
- `slog` (stdlib) vs `zerolog` vs custom — `slog` is idiomatic Go and supports
  both JSON and text handlers out of the box

### Internal API with Multiple Frontends

Faultbox should expose an internal API that multiple interfaces can consume:

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  Terminal    │  │  HTTP/gRPC  │  │  Future UI   │
│  (interactive│  │  API        │  │  (Tauri)     │
│   attach)   │  │  (headless) │  │              │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │                │                │
       └────────────────┼────────────────┘
                        │
                ┌───────▼───────┐
                │  Core Engine  │
                │  (Go API)     │
                │               │
                │ - Run()       │
                │ - Status()    │
                │ - Inspect()   │
                │ - InjectFault │
                └───────────────┘
```

**Interactive terminal (attach mode):** Connect to a running faultbox session,
query child process state, view execution plan, trigger faults manually.
Think `docker exec -it` but for fault injection sessions.

**Headless API:** Same operations exposed over HTTP/gRPC for programmatic
control — CI pipelines, the future desktop UI, or an AI agent orchestrating
experiments.

**Why early:** If the launcher in Step 1 is a monolithic `main()` that directly
calls `exec.Cmd`, there's nothing to attach to. Designing the core as a
library (`internal/engine`) with a clean Go API from the start means the CLI
is just one thin frontend. The API boundary also forces us to think about
what state is observable and what operations are possible — which directly
shapes the syscall/namespace/fault injection design.

**Key design questions:**
- Session model: is a "run" a long-lived session with an ID?
- State machine: what states can a session be in? (starting → running →
  injecting → completed)
- Event streaming: how does the attach client receive real-time updates?
  (Server-Sent Events, WebSocket, gRPC stream)
