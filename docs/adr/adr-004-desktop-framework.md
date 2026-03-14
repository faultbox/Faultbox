# ADR-004: Desktop Framework - Tauri vs Electron

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

## Context

For Phase 2 (Desktop), need to wrap web UI in desktop application.

## Options Considered

| Factor | Tauri | Electron |
|--------|-------|----------|
| Bundle size | ~5-10MB | ~150-200MB |
| Memory usage | ~30-50MB | ~200MB+ |
| Backend language | Rust | Node.js |
| Maturity | Maturing | Very mature |
| Major apps | Few | VS Code, Slack |
| Founder interest | Aligns with Rust goal | No benefit |

## Decision

**Tauri**

## Rationale

- Significantly smaller bundle (better UX for dev tool)
- Lower memory footprint
- Aligns with Rust learning goal
- Modern choice signals technical sophistication
- Still uses React/TypeScript for UI
- Performance benefits for visualization-heavy app

## Consequences

- Need to learn Tauri basics
- Smaller community for troubleshooting
- Some APIs less mature than Electron
