# ADR-002: Backend Language - Go vs Rust

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO), Principal Engineer

## Context

Need to choose primary backend language for CLI and core engine. Founder has Go experience, wants to learn Rust. eBPF ecosystem supports both.

## Options Considered

| Factor | Go | Rust |
|--------|-----|------|
| eBPF ecosystem | Strong (cilium/ebpf) | Strong (Aya) |
| Learning curve | Low (2-4 weeks) | High (6-12 months) |
| Founder proficiency | High | Learning |
| Claude Code support | Excellent | Good |
| Time to MVP | Faster | Slower |
| Performance | Very good | Excellent |
| Hiring (future) | Easier | Harder |

## Decision

**Go for MVP, Rust for performance-critical components later**

## Rationale

- Speed to MVP is critical for validation
- Founder's Go proficiency = 2-3x velocity
- eBPF in Go is proven (Cilium, Falco, Tetragon)
- Claude Code generates better Go code
- Can introduce Rust incrementally for simulation engine
- Reduces technical risk in early stages

## Consequences

- Potential rewrite of hot paths later
- Need to design clean interfaces for Rust integration
- Rust learning happens in parallel, not blocking
