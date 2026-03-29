# Faultbox Tutorial

Learn fault injection for distributed systems — from your first syscall fault to
testing real Postgres containers. Each chapter introduces one concept, explains
*why* it matters, and builds on the previous.

**Total time:** ~3 hours (15-30 min per chapter)

## Platform

Faultbox uses Linux's seccomp-notify, which requires kernel 5.6+.

| Platform | How to run |
|----------|-----------|
| **Linux** | Native. All commands run directly. |
| **macOS** | Via Lima VM. Setup: `make env-create && make env-start`. All faultbox commands run inside: `limactl shell faultbox-dev -- <command>` |

Most chapters note where Linux and macOS steps differ.
Docker (chapter 7 only) must run inside the Lima VM on macOS.

## Prerequisites

- Go 1.24+ installed
- For macOS: Lima (`brew install lima`)
- Docker (chapter 7 only)

## Build

```bash
make build          # faultbox CLI (host)
make demo-build     # cross-compile for Lima VM (linux/arm64)
```

## Chapters

| # | Title | Duration | You'll learn why |
|---|-------|----------|-----------------|
| 1 | [Your First Fault](01-first-fault.md) | 15 min | How the OS can lie to your program — and why that's useful |
| 2 | [Writing Your First Test](02-first-test.md) | 20 min | How to codify "this system works" as a repeatable spec |
| 3 | [Fault Injection in Tests](03-fault-injection.md) | 25 min | How to answer "what happens when X fails?" systematically |
| 4 | [Traces & Assertions](04-traces-assertions.md) | 25 min | How to prove *what actually happened* at the kernel level |
| 5 | [Exploring Concurrency](05-concurrency.md) | 25 min | How to find bugs that only appear under specific timing |
| 6 | [Monitors & Partitions](06-monitors-partitions.md) | 20 min | How to define safety properties that must always hold |
| 7 | [Containers](07-containers.md) | 30 min | How to test real infrastructure (Postgres, Redis) with the same tools |

Each chapter ends with exercises that push slightly beyond the lesson.
