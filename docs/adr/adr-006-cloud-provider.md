# ADR-006: Cloud Provider - AWS

**Date:** 2026-01-01
**Status:** Accepted
**Deciders:** CEO, Ilon (CTO)

## Context

Need to choose primary cloud provider for SaaS phase.

## Options Considered

1. **AWS** — Market leader, founder experience
2. **GCP** — Good Kubernetes, less eBPF friction
3. **Azure** — Enterprise focus
4. **Railway/Fly.io** — Simpler, faster start

## Decision

**AWS**

## Rationale

- Founder has extensive AWS experience (inDrive: 12 regions)
- eBPF requires EC2 (kernel access) — AWS has best EC2 options
- Most enterprise customers use AWS
- Comprehensive service ecosystem
- Good Terraform support

## Consequences

- Higher complexity than PaaS options
- Need EC2 for eBPF (not Fargate)
- AWS-specific knowledge required
