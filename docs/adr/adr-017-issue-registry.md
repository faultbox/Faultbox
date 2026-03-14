# ADR-017: Issue Registry & Version Management

**Date:** 2026-01-02
**Status:** Accepted
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

Traditional formal verification tools operate in binary mode: they stop on the first error found. This creates a fundamental problem for real-world engineering:

1. **Known issues exist** — Not every bug needs immediate fixing
2. **Planned fixes take time** — Teams may have issues scheduled for future sprints
3. **Discovery should continue** — Finding new issues matters more than re-reporting known ones
4. **Proposals need validation** — Before implementing fixes, teams want to verify the fix works

## Decision

Implement an **Issue Registry** system with **Version Management** that:
1. Tracks known issues with business context and decisions
2. Supports multiple handler versions (current state vs. proposed fixes)
3. Provides verification modes that can skip acknowledged issues
4. Enables comparison between versions to validate fixes

## Issue Lifecycle

```
NEW → ACKNOWLEDGED → PLANNED → RESOLVED
         │
      WONT_FIX
```

## Verification Modes

### Mode 1: AUDIT
Complete picture of all issues (known + unknown). Does NOT stop on acknowledged issues.

### Mode 2: DISCOVERY
Find only NEW issues, skip known ones. Useful for CI/CD, regular verification runs.

### Mode 3: PROPOSAL
Verify that a proposed version fixes its target issues. Checks that `resolves` issues are actually fixed and no new issues are introduced.

### Mode 4: COMPARISON
Compare two versions side-by-side. Shows structural differences, issue resolution/introduction, and behavioral equivalence.

## P Integration: Skipping Known Issues

Faultbox transforms P specs based on the Issue Registry — acknowledged issues are logged but don't fail verification; unknown issues trigger assertion failures.

## Consequences

### Positive
- Practical for real teams — acknowledges not all issues can/should be fixed immediately
- Enables continuous verification — CI/CD can run without false positives
- Supports technical debt management
- Validates fixes before implementation
- Enterprise differentiator

### Risks
- Complexity — more states to manage than simple pass/fail
- Gaming potential — teams might acknowledge too many issues

### Mitigations
- Acknowledgment requires justification
- Expiring acknowledgments option
- Metrics and reporting on acknowledged issues
- Audit mode always available for full picture
