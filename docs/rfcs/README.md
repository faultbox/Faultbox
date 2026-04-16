# Faultbox RFCs

Request for Comments for Faultbox v0.3.0 — Domain-Centric Verification Platform.

## Process

1. RFC drafted as markdown in this directory
2. Discussion on technical & DX details
3. Approved → implementation begins on branch `rfc/NNN-short-name`
4. Implementation merged → RFC status updated to Implemented

## Status Legend

- **Draft** — Open for discussion
- **Approved** — Ready for implementation
- **Implemented** — Merged to main
- **Withdrawn** — Abandoned

## v0.3.0 RFC Roadmap

### Phase 1: Scenario Composition (Foundation)

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| [RFC-001](0001-scenario-probes.md) | Scenarios as Probes & Fault Scenario Composition | Implemented | — |
| RFC-002 | `domain()` Primitive & Multi-Service Grouping | — | — |
| RFC-003 | Parallel Composition: `wait_all`, `wait_first`, `wait_n` | — | RFC-001 |

### Phase 2: Modularity & Tooling

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| RFC-004 | Package Format & `@scope/` Resolution | — | RFC-002 |
| RFC-005 | `dependency()` Builtin & Explicit Contracts | — | RFC-002 |
| RFC-006 | `faultbox check` — Static Type Analysis | — | RFC-001, RFC-002 |
| RFC-007 | `faultbox schema export` & VS Code Extension | — | RFC-006 |
| RFC-008 | HTML Report & Fault Matrix Output | — | RFC-001 |

### Phase 2b: Input Coverage

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| [RFC-013](0013-parameterized-scenarios.md) | Parameterized Scenarios & Value Generators | Draft | RFC-001 |

### Infrastructure

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| [RFC-014](0014-unix-socket-fd-passing.md) | Unix Socket FD Passing for Container Seccomp | Draft | — |
| [RFC-015](0015-container-reuse.md) | Container Reuse with Seed/Reset Lifecycle | Draft | — |
| [RFC-016](0016-new-protocols.md) | New Protocols: UDP, QUIC, HTTP/2, MongoDB, ClickHouse, Cassandra | Draft | — |

### Phase 3: Trace Equivalence & Specification Mining

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| RFC-009 | Independence Relation & Trace Quotienting | — | RFC-001 |
| RFC-010 | DPOR-Guided Explore Mode | — | RFC-009 |
| RFC-011 | `faultbox infer` — Specification Mining from Traces | — | RFC-009 |
| RFC-012 | `faultbox export --format p-lang` — P-lang Bridge | — | RFC-011 |

## Dependency Graph

```
Phase 1 (Foundation)         Phase 2 (Modularity)         Phase 3 (Theory)

RFC-001 Scenarios --------→ RFC-003 Parallel         RFC-009 Independence
    │                        RFC-006 Check              │           │
    │                        RFC-007 Schema+VSCode      RFC-010 DPOR  RFC-011 Infer
    │                        RFC-008 HTML Report                       │
    │                                                             RFC-012 P-lang
RFC-002 Domain -----------→ RFC-004 Packages
                             RFC-005 dependency()
```

## Ordering Rationale

RFC-001 and RFC-002 are independent foundations — they can proceed in parallel.

RFC-001 changes how scenarios work (return values, no inline assertions) and introduces
`fault_assumption()`, `fault_scenario()`, `fault_matrix()`. Most other RFCs build on this.

RFC-002 (`domain()`) enables the modularity story: packages (RFC-004) and dependency
declarations (RFC-005) need the domain concept.

Phase 3 (trace equivalence) depends on Phase 1 because the fault matrix naturally
produces the trace corpus that the equivalence engine operates on.
