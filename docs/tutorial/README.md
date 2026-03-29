# Faultbox Tutorial

Learn fault injection for distributed systems — from your first syscall fault to
testing real Postgres containers. Each chapter introduces one concept and builds
on the previous.

## Prerequisites

- **Go 1.24+** installed
- **Linux** with kernel 5.6+ (seccomp-notify). On macOS, use the Lima VM:
  ```bash
  make env-create   # one-time setup
  make env-start    # start VM
  ```
- **Docker** (chapter 7 only)

## Build

```bash
make build                    # faultbox CLI
make demo-build               # demo binaries (cross-compile for Lima VM)
```

For Lima VM, all commands run via:
```bash
limactl shell --workdir /host-home/git/Faultbox faultbox-dev -- <command>
```

## Chapters

| # | Title | Concept | You'll use |
|---|-------|---------|------------|
| 1 | [Your First Fault](01-first-fault.md) | `faultbox run`, syscall interception | `poc/target` binary |
| 2 | [Writing Your First Test](02-first-test.md) | Starlark specs, services, assertions | `poc/mock-db` |
| 3 | [Fault Injection in Tests](03-fault-injection.md) | `fault()`, `deny()`, `delay()`, multi-service | `poc/mock-api` + `poc/mock-db` |
| 4 | [Traces & Assertions](04-traces-assertions.md) | Temporal assertions, event log, ShiViz | `poc/demo` (order + inventory) |
| 5 | [Exploring Concurrency](05-concurrency.md) | `parallel()`, seed replay, exhaustive | `poc/demo` |
| 6 | [Monitors & Partitions](06-monitors-partitions.md) | `monitor()`, `partition()`, safety | `poc/demo` |
| 7 | [Containers](07-containers.md) | Docker, `image=`, `build=`, real Postgres | `poc/demo-container` |

Each chapter takes 15-30 minutes and ends with exercises.
