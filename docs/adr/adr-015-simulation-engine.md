# ADR-015: Simulation Engine Architecture

**Date:** 2026-01-02
**Status:** Proposed
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Define the architecture for Faultbox's deterministic simulation engine that runs real containers with controlled time and fault injection.

## To Be Defined

- Container orchestration (Docker? containerd?)
- Time virtualization approach
- Network simulation (eBPF-based)
- Fault injection primitives
- Trace collection
- Deterministic replay mechanism
- Resource isolation

## Two Verification Modes

### Mode 1: P Model Checking
- Exhaustive state space exploration
- Milliseconds to explore millions of states
- Finds design bugs
- Cannot model: actual network behavior, library bugs, resource exhaustion

### Mode 2: Deterministic Simulation
- Real containers, controlled environment
- Slower but catches implementation bugs
- Validates P-generated scenarios in real code
- Measures actual latencies

## eBPF Role

- Network interception and delay injection
- System call interception
- Trace collection without instrumentation
- Time virtualization hooks

*This ADR will be detailed during Phase 1-2 development.*
