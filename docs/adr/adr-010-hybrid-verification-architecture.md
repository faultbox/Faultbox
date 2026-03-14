# ADR-010: Hybrid Verification Architecture

**Date:** 2026-01-02
**Status:** Accepted
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)

## Context

Faultbox aims to make distributed systems more tangible through simulation, visualization, and experimentation. A key strategic differentiator is incorporating formal methods to provide intelligent, mathematically-grounded fault scenario generation — without requiring users to become formal methods experts.

The core problem: engineers find formal methods valuable in theory but impractical in practice due to:
1. **Cognitive overhead** — learning new syntax and mental models
2. **Disconnection problem** — specifications feel like "documentation that doesn't compile"
3. **Invisible ROI** — value only visible when outages *don't* happen
4. **Binary verification** — tools stop on first error, but real engineering needs nuanced issue management

## Strategic Goals

**Primary positioning:** "Chaos engineering, but smarter" with "Formal methods made accessible" as the competitive moat.

We sell the **outcome** (resilience, confidence, technical debt management), not the **method** (formal verification).

## Core Abstractions

### The Endpoint + Handler Model

The right abstraction is **not** "Service" but **Endpoint + Handler**. A handler captures ONLY the parts that can cause incorrect system behavior:
- External service calls
- Database operations
- Message queue publishes
- Error responses (4xx, 5xx)

Business logic (validation, calculations) is abstracted away.

### Protocol as Message Type

Protocols (HTTP, gRPC, Kafka) are message type abstractions for the actor model:

| Protocol | Type | Key Metadata |
|----------|------|-------------|
| HTTP | Request/Response | method, path, headers, status codes |
| gRPC | Request/Response | service, method, status codes |
| Kafka | Async | topic, partition, key, delivery guarantee |

## Architecture Overview

### Closed Verification Loop

```
Faultbox DSL (Endpoint + Handler specs)
    │                          ▲
    │ transpile to P           │ proposes specs
    ▼                          │
P Model Checker ──────► FaultboxTrace (scenarios)
                              │
                        execute in simulation
                              │
                              ▼
                      Deterministic Simulation
                              │
                              ▼
                        Issue Registry
                        (Known │ New)
                        (Skip    Report)
```

### Bidirectional Value

**Spec → Simulation (top-down):** P model checker finds edge cases → generate traces → replay in real containers

**Simulation → Spec (bottom-up):** User discovers behavior → capture trace → LLM proposes invariants

## System Layers

1. **Faultbox UI** — Visual interface for topology, flows, issue management ([ADR-011](adr-011-ui-architecture.md))
2. **Faultbox DSL** — Domain-specific language engineers actually want to use ([ADR-012](adr-012-specification-model.md))
3. **LLM Translation Layer** — Bridge natural language to formal specifications ([ADR-013](adr-013-llm-integration.md))
4. **P-lang Compiler Target** — Generated P code users never see ([ADR-014](adr-014-p-lang-integration.md))
5. **Verification & Simulation Engine** — Two modes: model checking + deterministic simulation ([ADR-015](adr-015-simulation-engine.md))
6. **Issue Registry & Version Management** — Track known issues and proposed fixes ([ADR-017](adr-017-issue-registry.md))

## Market Differentiation

| Competitor | Limitation | Faultbox Advantage |
|-----------|-----------|-------------------|
| Gremlin / LitmusChaos | Random injection, no intelligence | FM-powered scenario generation |
| Jepsen | Manual, requires expert | Democratized, automated |
| TLA+ tooling | No real system connection | Hybrid loop validates both |
| Datadog / Honeycomb | Reactive observability | Predictive, pre-production |
| **All FM tools** | Binary pass/fail, stop on error | **Issue management, version comparison** |

## Consequences

### Positive
- Unique market position — No one else has FM + simulation + issue management
- Practical for real teams — Acknowledges not all issues can/should be fixed immediately
- Defensible IP — DSL, P translation, LLM fine-tuning

### Risks
- P-lang dependency on external project
- Complexity of multiple verification modes
- LLM translation quality is critical

### Mitigations
- Keep P abstracted behind DSL (can swap if needed)
- Start simple, add complexity incrementally
- Heavy investment in LLM fine-tuning
