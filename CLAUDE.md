# Faultbox - AI Development Guidelines

## Project Overview

Faultbox is a distributed systems simulation and verification platform.
It helps engineers understand, experiment with, and validate distributed systems
by combining formal methods (P-lang) with interactive simulation and fault injection.

**Company:** Faultbox (under Purestack.ai)
**Mission:** Help engineers understand, experiment with, and validate distributed systems
**Positioning:** "Chaos engineering, but smarter" — formal methods made accessible

## Tech Stack

- **Backend/Core:** Go (primary), Rust (future performance-critical components)
- **Frontend:** React 18+ with TypeScript 5+ (Phase 2: Desktop)
- **Desktop Shell:** Tauri (Rust) (Phase 2)
- **Formal Verification:** P-lang (transpiled from Faultbox DSL, users never see P code)
- **Database:** SQLite (local/desktop), PostgreSQL (SaaS phase)
- **Infrastructure:** AWS (SaaS phase), local-only for Phase 1
- **eBPF:** cilium/ebpf (tracing and fault injection)

## Architecture

### Core Concepts

- **Endpoint + Handler model:** Specifications are built around endpoints and their handler
  steps (failure points only — external calls, DB ops, message queue publishes)
- **IR-First:** Go structs are the internal representation (source of truth);
  YAML/JSON are serialization formats, not the DSL
- **Hybrid Verification:** P model checker (design bugs, fast) + deterministic simulation
  (implementation bugs, slower) in a closed loop
- **Issue Registry:** Known issues can be acknowledged, planned, or resolved —
  verification modes (audit, discovery, proposal, comparison) skip acknowledged issues

### Project Structure

```
faultbox/
├── CLAUDE.md                 # This file
├── cmd/
│   ├── faultbox/             # CLI entrypoint
│   │   └── main.go
│   └── faultbox-shim/        # Container entrypoint shim (PoC 2)
│       └── main.go
├── internal/
│   ├── engine/               # Session lifecycle, fault rules, hold queues
│   ├── seccomp/              # BPF filter, shim, seccomp-notify API
│   ├── star/                 # Starlark runtime, builtins, event log
│   ├── container/            # Docker API wrapper (PoC 2, in progress)
│   ├── config/               # YAML topology/spec parsing
│   └── logging/              # Console/JSON structured logging
├── poc/
│   ├── demo/                 # PoC 1 demo: order-svc + inventory-svc
│   └── demo-container/       # PoC 2 demo: API + Postgres + Redis (planned)
├── docs/
│   ├── adr/                  # Architecture Decision Records
│   ├── poc/                  # PoC step documentation
│   ├── spec-language.md      # Starlark spec language reference
│   ├── cli-reference.md      # CLI reference
│   └── discovery.md          # Positioning & discovery document
└── Makefile
```

## Code Standards

- **Go:** Follow Effective Go, use `golangci-lint`
- **Tests:** Required for all new code (80%+ coverage target)
- **Error handling:** Wrap errors with `fmt.Errorf("context: %w", err)`
- **Patterns:** Repository pattern for data access, service layer for business logic
- **Context:** Always use `context.Context` for cancellation and tracing

## Architecture Principles

1. Simplicity over cleverness
2. Explicit over implicit
3. Composition over inheritance
4. Fail fast, fail loudly

## Git Workflow

- Branch naming: `feature/`, `bugfix/`, `hotfix/`
- Commit messages: Conventional Commits
- PR required for all changes to main

## Key ADRs

See `docs/adr/` for full details. Key decisions:

- **ADR-002:** Go for MVP, Rust for performance-critical components later
- **ADR-003/004:** React + TypeScript with Tauri for Desktop
- **ADR-010:** Hybrid Verification Architecture (P-lang + simulation)
- **ADR-012:** IR-First specification model (Go structs as source of truth)
- **ADR-014:** P-lang as core verification engine
- **ADR-017:** Issue Registry with nuanced verification modes
- **ADR-018:** Code-to-spec extraction from Go source
- **ADR-019:** Desktop-first MVP strategy (skip TUI)
- **ADR-020:** Two operating modes — Discovery (tracing) & Verification (invariants)

## Build & Test

```bash
make build      # Build binary to bin/faultbox
make test       # Run all tests
make lint       # Format + vet
make clean      # Remove build artifacts
make run        # Build and run
```
