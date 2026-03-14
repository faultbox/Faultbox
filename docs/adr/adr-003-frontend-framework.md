# ADR-003: Frontend Framework - React vs Uno Platform

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

## Context

Need to choose frontend technology for Desktop and SaaS phases. Uno Platform (C#/XAML) was considered for cross-platform native apps.

## Options Considered

| Factor | React + TypeScript | Uno Platform |
|--------|-------------------|--------------|
| Ecosystem size | Huge | Small |
| Claude Code support | Excellent | Limited |
| Hiring pool | Large | Small |
| Learning curve | Moderate | Steep (XAML) |
| Bundle size (web) | Small (~200KB-2MB) | Large (~10-30MB WASM) |
| Component libraries | Rich (shadcn, Radix) | Limited |
| Desktop option | Tauri, Electron | Native |

## Decision

**React + TypeScript with Tauri for Desktop**

## Rationale

- Web-first SaaS is the business goal
- Much faster iteration with Claude Code
- Larger ecosystem of components and examples
- Tauri provides excellent desktop experience with small bundle
- Easier to hire React developers later
- TypeScript provides type safety benefits

## Consequences

- Desktop requires Tauri wrapper (Rust)
- Not truly native UI, but acceptable for dev tools
- Aligns with Rust learning goal through Tauri
