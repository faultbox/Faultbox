# Faultbox RFCs

Request for Comments for Faultbox v0.3.0 — Domain-Centric Verification Platform.

## Process

1. RFC drafted as markdown in this directory
2. Discussion on technical & DX details
3. Approved → implementation begins
4. Each RFC maps to a branch: `rfc/NNN-short-name`

## Status Legend

- **Draft** — Open for discussion
- **Approved** — Ready for implementation
- **Implemented** — Merged to main
- **Withdrawn** — Abandoned

## v0.3.0 RFC Roadmap

### Phase 1: Scenario Composition (Foundation)

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| [RFC-001](0001-scenario-probes.md) | Scenarios as Probes & Fault Scenario Composition | Draft | — |
| RFC-002 | `domain()` Primitive & Multi-Service Grouping | Draft | — |
| RFC-003 | Parallel Composition: `wait_all`, `wait_first`, `wait_n` | Draft | RFC-001 |

### Phase 2: Modularity & Tooling

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| RFC-004 | Package Format & `@scope/` Resolution | — | RFC-002 |
| RFC-005 | `faultbox check` — Static Type Analysis | — | RFC-001, RFC-002 |
| RFC-006 | HTML Report & Fault Matrix Output | — | RFC-001 |

### Phase 3: Trace Equivalence

| RFC | Title | Status | Depends On |
|-----|-------|--------|------------|
| RFC-007 | Independence Relation & Trace Quotienting | — | RFC-001 |
| RFC-008 | DPOR-Guided Explore Mode | — | RFC-007 |
| RFC-009 | `faultbox infer` — Specification Mining from Traces | — | RFC-007 |

## Ordering Rationale

RFC-001 is the foundation — it changes how scenarios work (return values, no inline assertions) and introduces `fault_assumption()`, `fault_scenario()`, `fault_matrix()`. Everything else builds on this.

RFC-002 (`domain()`) is independent of RFC-001 but enables RFC-004 (packages).

Phase 3 (trace equivalence) depends on Phase 1 because the fault matrix naturally produces the trace corpus that the equivalence engine operates on.
