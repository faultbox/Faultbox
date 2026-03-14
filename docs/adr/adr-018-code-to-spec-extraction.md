# ADR-018: Code-to-Spec Extraction (Go MVP)

**Date:** 2026-01-02
**Status:** Proposed
**Deciders:** CEO (Boris), Ilon (CTO/Advisor)
**Parent ADR:** [ADR-010](adr-010-hybrid-verification-architecture.md)

## Context

The biggest barrier to formal methods adoption is the cold start problem: engineers must write specifications from scratch before seeing any value.

**Solution:** Extract specifications automatically from existing Go code. Engineers get immediate value (discovered failure points) and can refine the generated specs.

## Decision

Build a Go code analyzer that extracts Faultbox IR specifications from source code, focusing on:
1. HTTP/gRPC/Kafka endpoint detection
2. Handler analysis — identify external calls (failure points)
3. Error flow mapping — connect errors to response codes
4. IR generation — output to YAML/JSON

**MVP Scope:** Go only (most common for microservices, good AST tooling)

## What We Extract

Handler = sequence of failure points. We extract ONLY what can fail:

| Category | Go Patterns | IR Output |
|----------|------------|-----------|
| Database | `sql.DB`, `gorm.DB`, `sqlx.DB`, `pgx.Pool` | `call: Storage.Operation` |
| HTTP Client | `http.Client.Do()`, `resty.R().Get()` | `call: ExternalService.Endpoint` |
| gRPC Client | Generated `*Client` method calls | `call: Service.Method` |
| Kafka Producer | `sarama.Producer.SendMessage` | `publish: Kafka(topic)` |
| Redis/Cache | `redis.Client` method calls | `call: Cache.Operation` |
| Error Return | `return nil, err` | `on_error: ...` |

Business logic (validation, calculations, logging, metrics) is ignored.

## Confidence Levels

| Extraction | Confidence | Reason |
|-----------|-----------|--------|
| HTTP endpoint path/method | **High** | Explicit in router registration |
| External call detection | **High** | Type information available |
| Call ordering | **High** | AST gives source order |
| Error → status code mapping | **Medium** | Requires control flow analysis |
| Error type classification | **Low** | Needs heuristics or comments |
| Business logic assertions | **Low** | Hard to distinguish from validation |

Low-confidence extractions are marked with `# REVIEW:` comments.

## Architecture

```
Go Source Files
    │
    ▼
Go Parser + Type Info (go/ast, go/parser, go/types, golang.org/x/tools/go/packages)
    │
    ▼
Analyzer Pipeline: Endpoint Detector → Handler Analyzer → Error Mapper
    │
    ▼
Intermediate Representation (Go structs)
    │
    ▼
YAML/JSON output (editable spec)
```

## CLI Interface

```bash
faultbox extract ./...              # Extract from current directory
faultbox extract ./... --dry-run    # Show what would be extracted
faultbox extract ./... --merge      # Merge with existing spec
```

## Technical Dependencies

- `go/ast`, `go/parser`, `go/token`, `go/types`
- `golang.org/x/tools/go/packages`
- `golang.org/x/tools/go/ssa` (optional, for complex control flow)

## Consequences

### Positive
- Zero cold start — engineers get specs immediately
- Always in sync — re-extract to catch drift
- Discovery — shows failure points engineers didn't think about
- Competitive advantage — no other FM tool does this

### Risks
- False confidence — auto-generated specs may miss things
- Framework coverage — can't support every framework
