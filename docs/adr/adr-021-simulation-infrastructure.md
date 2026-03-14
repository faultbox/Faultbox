# ADR-021: Simulation Infrastructure - Four Layers

**Date:** 2026-01-03
**Status:** Draft

## Context

During UC-2 planning, we identified four critical capabilities for realistic pre-production validation:
1. **Service Configuration** — Real services need ENV vars, secrets, config files
2. **Environment Preparation** — Testing invariants requires pre-existing data
3. **Service Stubs** — Not all dependencies are available; need programmable mocks
4. **Deep Tracing** — External observation misses internal application context

## Decision

Faultbox will provide a **four-layer simulation infrastructure**:

### Layer 1: Environment (Configuration & Data Preparation)

Configure services (ENV variables, secrets, config files) and prepare test data (SQL scripts, fixture files, seed functions).

### Layer 2: Lambda-Services (Programmable Stubs)

Lightweight, programmable stubs that simulate external dependencies:
- Implement standard protocols (HTTP, gRPC, DB wire protocols, Kafka)
- Simple, declarative logic
- Emit traces like real services
- Can inject failures programmatically

### Layer 3: SDK (Deep Application Tracing)

Application SDK to emit rich traces from inside code, providing business context that external observation misses:
- Business context: "Processing order for VIP customer"
- State snapshots: "Cart contains 3 items, total $150"
- Custom spans: "Fraud check took 45ms, result: PASS"

Available for Go and TypeScript.

### Layer 4: Monitors (Real-time Invariant Checking)

Lambda-Monitors are live assertion checkers that can run against:
- Faultbox simulations (testing)
- Real staging/production systems (observability)

Similar to P-spec monitors, but for real distributed systems.

## MVP Phasing

### Phase 1: Desktop MVP (Month 2-3)
- Layer 1: ENV variables, basic SQL seed scripts
- Layer 2: HTTP stubs (simple request/response)
- Layer 3: Deferred
- Layer 4: Basic invariant checking only

### Phase 2: Full Infrastructure (Month 4-6)
- Full Lambda-Services with all protocols
- Go and TypeScript SDKs
- Real-time monitors
- Production monitoring capability

## Consequences

### Positive
- Complete simulation environment — not just observing, but controlling
- Realistic testing — proper config, data, dependencies
- Deep visibility — SDK traces show business context
- Production-ready monitors — same invariants work in simulation and production

### Negative
- Complexity — four layers is a lot to explain
- Setup overhead — more config than simple tracing
- SDK adoption — requires code changes

### Mitigation
- Start with Layer 1 + basic Layer 2 in MVP
- SDK is optional enhancement
- Monitors are advanced feature for later
- Good defaults minimize required config

## Related

- [ADR-020: Two Operating Modes](adr-020-two-operating-modes.md)
- [ADR-019: Desktop-First Strategy](adr-019-desktop-first-mvp.md)
