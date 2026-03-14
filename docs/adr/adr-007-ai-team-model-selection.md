# ADR-007: AI Team Model Selection

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

## Context

Need to assign Claude models to different AI team roles based on task complexity and cost.

## Decision

| Role | Model | Rationale |
|------|-------|-----------|
| Advisor/CTO (Ilon) | Opus 4.5 | Strategic thinking, complex analysis |
| Principal Engineer | Opus 4.5 | Architecture decisions, code review quality |
| Senior Engineers | Sonnet 4.5 | Production code, good balance of speed/quality |
| DevOps | Sonnet 4.5 | Infrastructure code, well-documented patterns |
| QA | Sonnet 4.5 | Test generation, structured output |
| Tech Writer | Sonnet 4.5 | Documentation, clear communication |

## Rationale

- Opus for decisions requiring deep reasoning
- Sonnet for execution tasks with clear patterns
- Cost optimization: Opus ~5x more expensive than Sonnet
- Sonnet 4.5 is highly capable for most coding tasks

## Consequences

- Need to route tasks to appropriate models
- May need to escalate complex issues to Opus
- Monitor quality and adjust if needed
