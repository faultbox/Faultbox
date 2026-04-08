# Faultbox - AI Development Guidelines

## Project Overview

Faultbox is a fault injection platform for distributed systems. It intercepts
syscalls via Linux's **seccomp-notify** mechanism to inject faults (deny, delay,
hold) into running services — both local binaries and Docker containers.

Tests are written in **Starlark** (Python-like): declare topology, start services,
inject faults, assert on behavior and syscall traces.

**Company:** Faultbox (under Purestack.ai)
**Mission:** Help engineers verify distributed systems behavior under failure
**Positioning:** "Chaos engineering, but smarter" — syscall-level fault injection
with deterministic replay and exhaustive interleaving exploration

## Tech Stack (Current)

- **Language:** Go 1.26+
- **Spec Language:** Starlark (go.starlark.net)
- **Syscall Interception:** seccomp-notify (kernel 5.6+, no eBPF)
- **Container Orchestration:** Docker Engine API (github.com/docker/docker)
- **Platform:** Linux-only (macOS via Lima VM)
- **CI:** GitHub Actions

## Architecture

### How It Works

```
Starlark Spec (.star)
    |
    v
Runtime (internal/star/) -- parses topology, discovers tests
    |
    +-- For each test:
        1. Start services (binary or Docker container)
        2. Install seccomp filter (via shim for containers)
        3. Wait for healthchecks
        4. Run test function (HTTP/TCP steps, fault injection)
        5. Notification loop processes intercepted syscalls
        6. Stop services, report trace
```

### Core Concepts

- **seccomp-notify:** Kernel mechanism that pauses a process on specific syscalls
  and asks a supervisor (faultbox) whether to allow, deny, or delay them
- **Starlark specs:** Single .star file declares services, interfaces, healthchecks,
  faults, and test functions. Configuration is code.
- **Event log:** Every intercepted syscall is recorded with service attribution,
  vector clocks, and decision. Temporal assertions query this log.
- **Per-service filtering:** Only services targeted by fault() get seccomp filters.
  Unfaulted services run at native speed.
- **Syscall family expansion:** `write=deny("EIO")` automatically covers write,
  writev, pwrite64. Users think in operations, not syscall numbers.

### Two Modes

| Mode | How | Use case |
|------|-----|----------|
| **Binary** | Fork+exec with seccomp filter | Local development, mock services |
| **Container** | Docker + faultbox-shim entrypoint | Real infrastructure (Postgres, Redis) |

### Project Structure

```
faultbox/
├── CLAUDE.md                 # This file
├── cmd/
│   ├── faultbox/             # CLI: test, run, generate, init, diff commands
│   └── faultbox-shim/        # Container entrypoint shim (Linux-only)
├── internal/
│   ├── engine/               # Session lifecycle, fault rules, hold queues, notification loop
│   ├── seccomp/              # BPF filter generation, seccomp-notify API, arch tables
│   ├── star/                 # Starlark runtime, builtins, event log, per-service filtering
│   ├── container/            # Docker API wrapper, network, container launch, pidfd_getfd
│   ├── protocol/             # Protocol plugins (http, tcp, postgres, redis, kafka, mysql, nats, grpc)
│   ├── eventsource/          # Event source plugins (stdout, wal_stream, topic, tail, poll) + decoders
│   ├── generate/             # Failure scenario generator (analyzer, matrix, codegen)
│   ├── config/               # YAML topology parsing (legacy)
│   └── logging/              # Console/JSON structured logging
├── poc/
│   ├── demo/                 # Binary demo: order-svc + inventory-svc (5 tests)
│   ├── demo-container/       # Container demo: Go API + Postgres + Redis (4 tests)
│   ├── mock-api/             # Simple HTTP API wrapping mock-db
│   ├── mock-db/              # Simple TCP key-value store
│   ├── target/               # Minimal binary for fault injection testing
│   └── example/              # Simple 2-service example spec
├── docs/
│   ├── tutorial/             # 12-chapter tutorial in 5 parts (prelude, first taste, syscall, protocol, advanced)
│   ├── design/               # Design documents (generator, etc.)
│   ├── use-cases/            # User persona stories (BE, QA, Mobile)
│   ├── adr/                  # Architecture Decision Records (historical)
│   ├── poc/                  # PoC step documentation
│   ├── spec-language.md      # Starlark spec language reference
│   ├── cli-reference.md      # CLI reference
│   ├── errno-reference.md    # Error code reference for fault injection
│   └── discovery.md          # Positioning document
├── .github/workflows/ci.yml  # GitHub Actions: build, vet, test, cross-compile
└── Makefile
```

### Key Packages

| Package | Purpose | Key files |
|---------|---------|-----------|
| `internal/engine` | Session lifecycle, fault rule matching, notification loop | `session.go`, `launch_linux.go`, `fault.go` |
| `internal/seccomp` | BPF filter building, seccomp syscall, arch tables | `filter_linux.go`, `arch_arm64.go`, `arch_amd64.go` |
| `internal/star` | Starlark runtime, all builtins, event log, service lifecycle | `runtime.go`, `builtins.go`, `types.go` |
| `internal/container` | Docker client, network, container launch with shim | `docker.go`, `launch.go`, `fd_linux.go` |
| `internal/protocol` | Protocol plugins (http, tcp, postgres, redis, kafka, etc.) | `protocol.go`, `http.go`, `postgres.go` |
| `internal/eventsource` | Event source plugins (stdout, wal_stream, topic, tail, poll) | `eventsource.go`, `stdout.go`, `walstream.go` |
| `internal/generate` | Failure scenario generator (topology analysis → mutations) | `analyzer.go`, `matrix.go`, `codegen.go` |

## Code Standards

- **Go:** Follow Effective Go, use `go vet`
- **Tests:** Required for all new code. Currently 103 tests, 4 E2E container tests.
- **Error handling:** Wrap errors with `fmt.Errorf("context: %w", err)`
- **Context:** Always use `context.Context` for cancellation
- **Readability:** Optimize code and structure for LLM consumption (Claude), not just humans
- **Build tags:** Linux-only files use `//go:build linux`. Cross-platform stubs in `*_other.go`.

## Architecture Principles

1. Simplicity over cleverness
2. Explicit over implicit
3. Composition over inheritance
4. Fail fast, fail loudly

## Git Workflow

- Branch naming: `feature/`, `bugfix/`, `docs/`
- Commit messages: Conventional Commits (`feat:`, `fix:`, `perf:`, `docs:`, `ci:`)
- PR required for all changes to main
- CI must pass (build + vet + test + cross-compile)

## Build & Test

```bash
# Host (macOS/Linux)
make build          # Build bin/faultbox
make test           # Run all tests (go test ./...)
make lint           # Format + vet
make clean          # Remove build artifacts

# Cross-compile for Lima VM (linux/arm64)
make demo-build     # Build faultbox + faultbox-shim + demo binaries

# Run demos
make demo           # PoC 1: binary demo in Lima VM
make demo-container # PoC 2: container demo in Lima VM

# Lima VM management
make env-create     # Create faultbox-dev VM
make env-start      # Start VM
make env-stop       # Stop VM
make env-verify     # Verify kernel features
```

## Historical ADRs

The `docs/adr/` directory contains Architecture Decision Records from early
design phases. Many reference features that were superseded by the current
seccomp-notify + Starlark approach:

- **ADR-002:** Go for MVP (still current)
- **ADR-003/004:** React + Tauri Desktop (deferred — CLI-first approach taken)
- **ADR-010/014:** P-lang verification (superseded by Starlark + seccomp-notify)
- **ADR-012:** IR-First model (superseded by Starlark as single source of truth)
- **ADR-019:** Desktop-first strategy (superseded by CLI-first)
- **ADR-020:** Discovery & Verification modes (partially implemented via tracing)

These ADRs are kept for historical context but do not reflect current architecture.
