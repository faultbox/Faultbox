# ADR-020: Two Operating Modes - Discovery & Verification

**Date:** 2026-01-03
**Status:** Draft

## Context

Faultbox serves two distinct user needs:
1. **"What happens when I call this endpoint?"** → Discovery
2. **"Prove this invariant always holds"** → Verification

These modes share infrastructure but have fundamentally different UX flows and outputs.

## Decision

Faultbox will support **two operating modes** with a shared setup flow.

### Mode 1: Discovery (Tracing)

**Purpose:** Observe and understand system behavior through detailed tracing.
**User Action:** Select endpoint → Send request → See full trace
**Output:** Single execution trace with all HTTP/gRPC calls, DB queries, Kafka messages, timing breakdown.

**Use When:** Learning how the system works, debugging, understanding dependencies, quick exploration.

### Mode 2: Verification (Invariants)

**Purpose:** Prove system properties hold under failure conditions.
**User Action:** Define invariants → Add failure modes → Run simulation → Get report
**Output:** Multi-path verification report with pass/fail per invariant, counterexamples, path coverage, JUnit/CI export.

**Use When:** Pre-production validation, CI/CD integration, proving correctness, compliance.

## Invariant Language Design

Simplified temporal logic — users shouldn't need to learn LTL/CTL:

```yaml
invariants:
  # Always (Box operator)
  - name: "Max 3 bids per order"
    always: "COUNT(bids WHERE order_id = $order_id) <= 3"

  # Never
  - name: "No duplicate bids"
    never: "EXISTS duplicate IN bids GROUP BY (order_id, courier_id)"

  # Eventually with timeout
  - name: "Bid eventually produces Kafka event"
    after: "INSERT INTO bids"
    eventually: "PRODUCE TO bids.created"
    within: "5s"
```

## Failure Injection Model

Supported types: latency, timeout, error (HTTP status), db_timeout, db_deadlock, kafka_lag, kafka_duplicate — each with target, probability, and duration parameters.

## Related

- [ADR-019: Desktop-First Strategy](adr-019-desktop-first-mvp.md)
- [ADR-018: Code-to-Spec Extraction](adr-018-code-to-spec-extraction.md)
