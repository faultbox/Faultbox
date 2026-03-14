# ADR-016: Trace Format Specification

**Date:** 2026-01-02
**Status:** Proposed
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Define the canonical trace format (`FaultboxTrace`) that serves as the interchange format between P model checking, simulation engine, and production systems.

## To Be Defined

- Trace data model
- Serialization format (protobuf? JSON? custom?)
- P trace → FaultboxTrace conversion
- OpenTelemetry → FaultboxTrace conversion
- FaultboxTrace → simulation replay
- Timing representation
- Causality representation
- Compression strategy

## Design Principles

Analogous to LLVM IR for compilers — a stable intermediate representation that:

1. **P can emit** (or we translate P traces into)
2. **Simulation can replay** (deterministically)
3. **Production can emit** (for reverse-direction analysis)
4. **Supports timing** (even if P model doesn't fully use it)
5. **Human-readable** (for debugging)
6. **Efficient** (for large traces)

## Relationship to OpenTelemetry

- **Import:** Convert OTel traces to FaultboxTrace for analysis
- **Export:** Optionally emit OTel-compatible spans from simulation
- **Not replace:** FaultboxTrace is for verification, OTel is for observability

*This ADR will be detailed during Phase 1 (CLI) development.*
