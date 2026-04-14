# Faultbox Tutorial

Learn fault injection for distributed systems — from your first syscall
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

## Part 2: Syscall-Level Fault Injection

| # | Chapter | Duration |
|---|---------|----------|
| 3 | [Fault Injection in Tests](02-syscall-level/03-fault-injection.md) | 25 min |
| 4 | [Traces & Assertions](02-syscall-level/04-traces.md) | 25 min |
| 5 | [Exploring Concurrency](02-syscall-level/05-concurrency.md) | 25 min |
| 6 | [From Tests to Domains](02-syscall-level/06-domain-model.md) | 20 min |

## Part 3: Protocol-Level Fault Injection

| # | Chapter | Duration |
|---|---------|----------|
| 7 | [HTTP Protocol Faults](03-protocol-level/07-http-redis.md) | 25 min |
| 8 | [Database & Broker Faults](03-protocol-level/08-databases.md) | 25 min |

## Part 4: Safety & Verification

| # | Chapter | Duration |
|---|---------|----------|
| 14 | [Invariants & Safety Properties](04-safety/14-invariants.md) | 30 min |
| 15 | [Monitors & Temporal Properties](04-safety/15-monitors.md) | 30 min |
| 16 | [Network Partitions](04-safety/16-partitions.md) | 20 min |

## Part 5: Advanced Features

| # | Chapter | Duration |
|---|---------|----------|
| 9 | [Containers](05-advanced/09-containers.md) | 30 min |
| 10 | [Scenarios & Generation](05-advanced/10-scenarios.md) | 20 min |
| 11 | [Event Sources & Observability](05-advanced/11-event-sources.md) | 25 min |
| 12 | [Named Operations](05-advanced/12-named-ops.md) | 15 min |
| 13 | [LLM Agents & MCP](05-advanced/13-llm-mcp.md) | 15 min |

---

Each chapter ends with exercises that push slightly beyond the lesson.
