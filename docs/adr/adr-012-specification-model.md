# ADR-012: Faultbox Specification Model (IR-First)

**Date:** 2026-01-03
**Status:** Accepted
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Faultbox needs a way to represent system specifications that can be:
1. **Extracted** from existing Go code
2. **Edited** by engineers (refinements, known issues)
3. **Transpiled** to P-lang for verification
4. **Versioned** alongside code

### DSL Format Decision

YAML was initially considered but has significant problems (whitespace-sensitive, no type safety, poor error messages, no composition, not programmer-friendly).

**Decision:** IR-first approach. Defer DSL syntax until we have real user feedback.

## Decision: IR-First Architecture

### MVP Approach

```
Go Source ──► [Go Structs (IR)] ──► P-lang
                    │
                    ├──► YAML export (human-readable)
                    └──► JSON export (tooling)

IR = Internal Representation (source of truth)
YAML/JSON = Serialization formats (not the DSL)
```

### Future: Proper DSL

```
Go Source ──► [Go Structs (IR)] ◄──► Faultbox DSL
                    │
                    └──► P-lang

DSL syntax TBD based on user feedback
Candidates: Go-like, OCaml-like, LISP-like
```

## Internal Representation (IR)

Core Go types: `Spec`, `Service`, `Endpoint`, `Handler`, `HandlerStep`, `CallStep`, `PublishStep`, `AssertStep`, `ReturnStep`, `KnownIssue`, `Invariant`, `Dependency`.

Key types:
- `Protocol`: HTTP, gRPC, Kafka
- `StepType`: call, publish, assert, return
- `IssueStatus`: new, acknowledged, planned, resolved, wont_fix
- `VersionStatus`: production, proposed, deprecated, archived
- `ConfidenceLevel`: high, medium, low

## P-lang Mapping

| IR Element | P Construct |
|-----------|------------|
| Service | Module |
| Endpoint | Event type + handler machine |
| Handler | State machine with transitions |
| CallStep | send + receive pattern |
| PublishStep | send (fire-and-forget) or announce |
| KnownIssue | Transformed conditional (skip if acknowledged) |
| Invariant | Spec machine |

## Consequences

### Positive
- **Faster MVP** — No DSL parser needed initially
- **Informed decision** — DSL syntax based on real usage
- **Clean architecture** — IR separates concerns

### Risks
- **YAML editing UX** — Temporary pain for early users
- **Deferred decision** — Must eventually commit to DSL
