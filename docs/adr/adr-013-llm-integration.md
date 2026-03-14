# ADR-013: LLM Integration Architecture

**Date:** 2026-01-02
**Status:** Proposed
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Define how LLMs will be integrated to bridge natural language and formal specifications.

## To Be Defined

- Model selection (Claude API? Fine-tuned model?)
- Natural language → DSL translation pipeline
- DSL → Natural language explanation
- Invariant suggestion from topology
- Spec refinement when simulation diverges
- Training data strategy
- Evaluation metrics
- Cost optimization

## Key Use Cases

1. **Invariant authoring:** "Orders should never be double-charged" → formal property
2. **Counterexample explanation:** P trace → human-readable scenario
3. **Spec suggestion:** Given topology, suggest common invariants
4. **Refinement:** Simulation shows behavior X, propose spec update

*This ADR will be detailed during Phase 3 (SaaS) development.*
