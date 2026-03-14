# ADR-019: Desktop-First MVP Strategy

**Date:** 2026-01-03
**Status:** Accepted

## Context

Original plan was CLI-first (Month 1-3), then Desktop (Month 4-6). During use case development, we identified that the core value proposition — **interactive simulation with visual feedback** — is fundamentally a visual experience.

Key insight: Users need an "intervention layer" to interact with their system under simulation. In CLI/TUI this is awkward; in Desktop it's natural and powerful.

## Decision

**Desktop-first with minimal CLI support.**

### New Timeline

- **Month 1:** Foundation + Basic CLI (non-interactive) — core simulation engine (Go), `faultbox init`, `run` (batch mode only), code-to-spec extraction, no TUI investment
- **Month 2-3:** Desktop MVP (Interactive) — Tauri + React shell, visual endpoint picker, request/response visualization, real-time simulation view
- **Month 4:** Polish + CI/CD — JUnit output for CLI, desktop refinements, cross-platform builds

### What Changes

| Aspect | Old Plan | New Plan |
|--------|----------|----------|
| MVP Interface | CLI with TUI | Desktop app |
| TUI Development | 2-3 weeks | **Skip entirely** |
| Onboarding UX | Terminal-based | Visual, click-based |

## Rationale

1. **The "Aha Moment" is Visual** — Seeing request flow through services with timing breakdown is dramatically better as GUI
2. **Intervention Layer UX** — Desktop: click endpoint → see result instantly, zero friction
3. **Avoid Throwaway Work** — TUI would be 2-3 weeks then rebuilt for Desktop anyway
4. **Differentiation** — Starting visual-first is unusual and compelling

## Consequences

### Positive
- Better first impression for new users
- Faster path to "aha moment"
- No wasted TUI effort

### Negative
- Longer time to first external feedback (~1 month delay)
- More complex initial development (Tauri + React)

## Related

- [ADR-004: Desktop Framework - Tauri](adr-004-desktop-framework.md)
- [ADR-005: Product Evolution Path](adr-005-product-evolution-path.md) — Superseded by this decision
