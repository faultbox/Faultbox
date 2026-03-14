# ADR-005: Product Evolution Path

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

> **Note:** Superseded by [ADR-019](adr-019-desktop-first-mvp.md) for MVP strategy (Desktop-first instead of CLI-first), but the overall progression remains valid.

## Context

Need to determine product development sequence to maximize learning while minimizing risk.

## Options Considered

1. **Web SaaS first** — Direct to cloud product
2. **Desktop first** — Native app, then cloud
3. **CLI first** — Developer tool progression
4. **All at once** — Parallel development

## Decision

**CLI → Desktop → SaaS → Cloud Integrations**

## Rationale

### Phase 1: CLI
- Fastest to build (no UI complexity)
- Power users adopt good CLIs
- CI/CD ready from day one
- Forces clean architecture

### Phase 2: Desktop
- Visual layer on proven core
- Local-first (no infra costs)
- Broader user appeal

### Phase 3: SaaS
- Team collaboration features
- Recurring revenue model

### Phase 4: Cloud Integrations
- Enterprise features
- AWS/Azure/GCP native

## Consequences

- Slower path to recurring revenue
- Need to maintain multiple interfaces
- Core engine must be well-abstracted from start
