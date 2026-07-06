# Faultbox Tutorial

Learn fault injection for distributed systems - from your first syscall
fault to protocol-level proxy injection. Each chapter introduces one
concept, explains *why* it matters, and builds on the previous.

**Total time:** ~5 hours (15-30 min per chapter)

## Quick start

```bash
curl -fsSL https://faultbox.io/install.sh | sh    # install faultbox
git clone https://github.com/faultbox/demo.git    # clone demo services
cd demo
make build                                         # Linux
make lima-create && make lima-build                 # macOS (one-time)
```

## Platform

| Platform | How to run |
|----------|-----------|
| **Linux** | `faultbox test first-test.star` |
| **macOS** | `make lima-run CMD="faultbox test first-test.star"` |

---

## Part 0: Prelude & Configuration

| # | Chapter | Duration |
|---|---------|----------|
| 0 | [Setup](00-prelude/00-setup.md) | 10 min |

## Part 1: First Taste

| # | Chapter | Duration |
|---|---------|----------|
| 1 | [Your First Fault](01-first-taste/01-first-fault.md) | 15 min |
| 2 | [Writing Your First Test](01-first-taste/02-first-test.md) | 20 min |

## Part 2: Faults, Traces & Domains

| # | Chapter | Duration |
|---|---------|----------|
| 3 | [Fault Injection in Tests](02-syscall-level/03-fault-injection.md) | 25 min |
| 4 | [Traces & Assertions](02-syscall-level/04-traces.md) | 25 min |
| 5 | [Exploring Concurrency](02-syscall-level/05-concurrency.md) | 25 min |
| 6 | [From Tests to Domains](02-syscall-level/06-domain-model.md) | 20 min |

## Part 3: Simulate the Boundary

Mock the dependencies you can't run - the fastest path to testing a
real service whose dependencies belong to other teams.

| # | Chapter | Duration |
|---|---------|----------|
| 17 | [Mock Services](05-advanced/17-mock-services.md) | 25 min |
| 18 | [Typed gRPC Mocks](05-advanced/18-typed-grpc-mocks.md) | 20 min |
| 19 | [OpenAPI Mocks](05-advanced/19-openapi-mocks.md) | 20 min |
| 21 | [JWT/JWKS Mocks](05-advanced/21-jwt-mocks.md) | 15 min |

## Part 4: Real Infrastructure

| # | Chapter | Duration |
|---|---------|----------|
| 7 | [HTTP Protocol Faults](03-protocol-level/07-http-redis.md) | 25 min |
| 8 | [Database & Broker Faults](03-protocol-level/08-databases.md) | 25 min |
| 9 | [Containers](05-advanced/09-containers.md) | 30 min |

## Part 5: Safety & Verification

| # | Chapter | Duration |
|---|---------|----------|
| 14 | [Invariants & Safety Properties](04-safety/14-invariants.md) | 30 min |
| 15 | [Monitors & Temporal Properties](04-safety/15-monitors.md) | 30 min |
| 16 | [Network Partitions](04-safety/16-partitions.md) | 20 min |
| 10 | [Scenarios & Fault Matrix](05-advanced/10-scenarios.md) | 20 min |
| 24 | [Determinism & the L1 Contract](04-safety/24-determinism.md) | 25 min |
| 25 | [Non-deterministic Operators - `choose`, `assume`, `halt`](04-safety/25-choose-and-assume.md) | 30 min |
| 26 | [Plan-Tree Fan-Out - `faultbox plan`, probability, interleavings](04-safety/26-plan-fanout.md) | 30 min |

## Part 6: Power Tools

| # | Chapter | Duration |
|---|---------|----------|
| 11 | [Event Sources & Observability](05-advanced/11-event-sources.md) | 25 min |
| 12 | [Named Operations](05-advanced/12-named-ops.md) | 15 min |
| 13 | [LLM Agents & MCP](05-advanced/13-llm-mcp.md) | 15 min |
| 20 | [.fb Bundles](05-advanced/20-bundles.md) | 20 min |
| 22 | [End-to-End Go Microservice](05-advanced/22-go-microservice-end-to-end.md) | 40 min |
| 23 | [Reading Reports](05-advanced/23-reports.md) | 20 min |

> Chapter numbers reflect the order chapters were written, not the
> reading order - follow the parts top to bottom.

---

Each chapter ends with exercises that push slightly beyond the lesson.
