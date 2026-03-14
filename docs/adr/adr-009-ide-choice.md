# ADR-009: IDE Choice - VS Code + Claude Code

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

## Context

Need to choose primary IDE for Faultbox development. Key requirements: Go + eBPF support, React/TypeScript support, Remote SSH (critical for Lima VM / EC2), AI integration.

## Options Considered

| Factor | VS Code | Cursor | Zed |
|--------|---------|--------|-----|
| Go support | Excellent | Excellent | Good |
| Remote SSH | Excellent (best) | Excellent | Limited |
| AI integration | Extensions | Native | Native |
| Performance | Heavy | Heavy | Fast |
| Extensions | Huge | VS Code compatible | Limited |
| Cost | Free | $20/mo | Free |

## Decision

**VS Code + Claude Code (terminal)**

## Rationale

1. **Remote SSH is critical** — VS Code has best-in-class Remote SSH for Lima VM and EC2
2. **Claude Code handles AI** — No need to pay for Cursor
3. **Zero cost**
4. **Proven stack** — Mature, well-tested, huge community
5. **Best Go tooling** — gopls integration, debugging, test runner

## Workflow

```
VS Code: Visual editing, debugging, git, Remote SSH
Claude Code: Architecture, complex refactoring, code generation
```

## Consequences

- Need to context-switch between VS Code and terminal
- Heavier than Zed (Electron-based)
- May reconsider Zed when Remote SSH matures
