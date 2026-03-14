# ADR-014: P-lang Integration

**Date:** 2026-01-02
**Status:** Accepted
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Faultbox needs a formal verification foundation that:
1. Maps naturally to distributed systems / microservices
2. Generates actionable counterexample traces for fault injection
3. Is accessible to engineers (not just formal methods experts)
4. Has proven production track record
5. Supports compositional specification building

## Decision

**Use P-lang as the core verification engine**, with Faultbox DSL transpiling to P code that users never see directly.

## Why P Over Alternatives

| Criterion | TLA+ | Quint | P | Winner |
|----------|------|-------|---|--------|
| Syntax accessibility | Mathematical | Modern, TS-like | C-like, imperative | P/Quint |
| Maps to microservices | Abstract | Protocol-focused | Actor model | **P** |
| Code generation | None | None | C#, C deployable | **P** |
| Counterexample traces | Yes | Yes | Yes + reproducible replay | **P** |
| Production track record | Amazon (design) | Limited | AWS S3/DynamoDB, Microsoft Windows | **P** |

**Key differentiator:** P's actor model semantics directly match how engineers think about services communicating via messages/queues.

## P Capabilities We Will Use

1. **Explicit-State Model Checking** (Phase 1) — Fast bug-finding via PCT heuristics
2. **Compositional Module System** (Phase 1-2) — Maps to DSL service composition
3. **Spec Machines as Monitors** (Phase 1-2) — Express safety and liveness properties
4. **P Verifier / Inductive Proofs** (Phase 3-4) — Premium feature for enterprise
5. **PObserve Runtime Monitoring** (Phase 4) — Bridge to production monitoring

## Architecture

```
Faultbox DSL
    │ Transpiler
    ▼
Generated P Code (users never see this)
    │
    ├──► P Model Checker (PCT)
    ├──► P Verifier (Proofs)
    └──► PSym Symbolic (Future)
         │
         ▼
    Counterexample Traces → FaultboxTrace Format → Deterministic Simulation
```

## Distribution Strategy

- **Phase 1:** Bundle pre-built P compiler binaries for Linux/macOS/Windows
- **Phase 3+:** Consider embedding P compiler for tighter integration
- Dependencies: .NET Runtime (P compiler is C#-based), Z3 SMT Solver (for P Verifier, optional in Phase 1)

## DSL → P Transpiler Design Principles

1. One-way, deterministic — DSL always produces same P code
2. Readable output — Generated P includes comments for debugging
3. Composition-preserving — DSL modules → P modules
4. Incremental — Change one service, regenerate only that module

## Consequences

### Positive
- Proven foundation — 10+ years production use at Microsoft and AWS
- Natural mapping — Actor model fits microservices perfectly
- Actionable output — Reproducible traces become fault injection scenarios
- Growth path — From bug-finding (Phase 1) to formal proofs (Phase 3)

### Risks
- Dependency on external project
- Complexity — Transpiler must handle all DSL constructs correctly
- .NET dependency — Adds to distribution size

### Mitigations
- Keep P abstracted behind DSL (can swap if needed)
- Comprehensive transpiler test suite with golden P output files
- Bundle .NET runtime or use native AOT compilation (future)
