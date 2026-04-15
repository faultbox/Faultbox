# Chapter 9: Containers — Real Infrastructure

**Duration:** 30 minutes
**Prerequisites:** [Chapter 0 (Setup)](../00-prelude/00-setup.md) completed, Docker running

## Goals & Purpose

Chapters 1-6 used mock binaries — lightweight, fast, controllable. But in
production, your services talk to Postgres, Redis, Kafka, Elasticsearch —
real infrastructure with real complexity.

The question is: **does your error handling work against real Postgres, or
just against your mock?** A mock database returns errors instantly and
predictably. Real Postgres has connection pools, WAL persistence, buffer
management, and its own error handling. The failure modes are different.

Faultbox solves this by running real Docker containers with the same seccomp
fault injection used for binaries. The same `fault_assumption()` and
`fault_scenario()` API, the same assertions, the same trace output — but
now against real infrastructure.

This chapter teaches you to:
- **Orchestrate Docker containers** with Faultbox (instead of docker-compose)
- **Inject faults into real databases** — Postgres, Redis
- **Understand container networking** — how services find each other
- **Use per-service filtering** — only intercept syscalls on faulted services

After this chapter, you'll be able to answer: "what happens to my API when
Postgres has a disk I/O error?" — tested against real Postgres, not a mock.

## How container mode works

```
Host (faultbox)                          Docker Container
+-----------------------+    +----------------------------------+
| 1. Pull/build image   |    | faultbox-shim (bind-mounted)     |
| 2. Override entrypoint |    |   +- Install seccomp filter      |
|    with shim           |    |   +- Report listener fd          |
| 3. Acquire fd via      |<---|   +- Wait for host ACK           |
|    pidfd_getfd()       |    |   +- exec(original entrypoint)   |
| 4. Run notification    |    |                                  |
|    loop (same as       |    | Real service runs with           |
|    binary mode)        |    | seccomp filter active            |
+-----------------------+    +----------------------------------+
```

A tiny shim binary is injected into the container. It installs the seccomp
filter, then exec's the original entrypoint. From Postgres's perspective,
nothing changed — except faultbox can now intercept its syscalls.

## Prerequisites

Container tests require Docker and `sudo` (seccomp on containers needs
`pidfd_getfd` with `PTRACE_MODE_ATTACH`).

**macOS (Lima VM):** Docker is pre-installed in the Lima VM.

```bash
make lima-start                 # start VM if stopped
```

**Linux:** Install Docker normally.

```bash
docker version                  # verify Docker is available
# Docker must be running
# sudo required for pidfd_getfd across Docker PID namespaces
```

## Container services

Instead of a binary path, use `image=` or `build=`:

```python
# Pull from registry — no Dockerfile needed
postgres = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432", timeout="60s"),
)

# Build from local Dockerfile
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {
        "PORT": "8080",
        "DATABASE_URL": "postgres://postgres:test@" + postgres.main.internal_addr + "/testdb?sslmode=disable",
    },
    depends_on = [postgres],
    healthcheck = http("localhost:8080/health", timeout="60s"),
)
```

Three service sources — exactly one required:

| Parameter | When to use |
|-----------|-------------|
| `"/path/to/binary"` (positional) | Local development, mock services |
| `image = "postgres:16"` | Real infrastructure from Docker Hub |
| `build = "./api"` | Your service with a Dockerfile |

## Container networking

Containers run on a Docker bridge network (`faultbox-net`). Two addressing
modes:

| Attribute | Returns | Used by |
|-----------|---------|---------|
| `.internal_addr` | `"postgres:5432"` | Container-to-container env vars |
| `.addr` | `"localhost:32847"` | Faultbox healthchecks and test steps |

**Why two?** Inside Docker, containers reach each other by hostname
(`postgres:5432`). Outside Docker, faultbox reaches them via mapped ports
(`localhost:32847`). The `.internal_addr` attribute handles this automatically.

```python
# Container-to-container: use internal_addr
env = {"DATABASE_URL": "postgres://test@" + postgres.main.internal_addr + "/db"}

# Test steps (from host): addr is used automatically
resp = api.get(path="/health")  # hits localhost:32847
```

## Run the demo

**Linux:**
```bash
sudo faultbox test container-demo/faultbox.star
```

**macOS (Lima):**
```bash
make lima-run CMD="sudo faultbox test container-demo/faultbox.star"
```

```
--- PASS: happy_path (9.7s) ---
--- PASS: postgres_write_enospc (10.4s) ---
--- PASS: postgres_write_failure (9.4s) ---
--- PASS: write_and_read (9.7s) ---
4 passed, 0 failed
```

## Fault injection on containers

The domain-centric model separates scenarios, faults, and expectations.
Faultbox doesn't care if it's a binary or container:

```python
# --- Scenario: probe function returning an observable ---

def write_data():
    return api.post(path="/data?key=full&value=test")
scenario(write_data)

# --- Fault assumption: named, reusable ---

pg_disk_full = fault_assumption("pg_disk_full",
    target = postgres,
    write = deny("ENOSPC", label="disk full"),
)

# --- Fault scenario: composed test ---

fault_scenario("postgres_disk_full",
    scenario = write_data,
    faults = pg_disk_full,
    expect = lambda r: assert_true(r.status >= 500, "expected 5xx on ENOSPC"),
)
```

**Linux:**
```bash
sudo faultbox test container-demo/faultbox.star --test postgres_disk_full
```

**macOS (Lima):**
```bash
make lima-run CMD="sudo faultbox test container-demo/faultbox.star --test postgres_disk_full"
```

```
--- PASS: postgres_disk_full (10.4s, seed=0) ---
  fault rule on postgres: write=deny(ENOSPC) → filter:[write,writev,pwrite64] label="disk full"
    #5319  postgres  pwrite64  deny(no space left on device)  [disk full]
```

This denies `write`, `writev`, and `pwrite64` on the Postgres container.
When Postgres tries to write a data page (via `pwrite64`), it gets ENOSPC.
The SQL query fails, the API returns 503.

The diagnostic output shows the expansion:
```
  fault rule on postgres: write=deny(ENOSPC) -> filter:[write,writev,pwrite64]
    #5319  postgres  pwrite64  deny(no space left on device)
```

**Note:** Postgres uses `pwrite64` for data pages, not `write`. The syscall
family expansion handles this automatically — you write `write=deny(...)` and
it covers all write variants.

## Per-service filtering

Faultbox only installs seccomp filters on services that are actually faulted.
In the demo:
- **happy_path**: no faults → all containers run at native speed
- **postgres_write_failure**: only Postgres gets a seccomp filter

Redis and the API always run without interception overhead. This is why
fault tests and non-fault tests have similar timing (~10s).

## Why sudo?

`pidfd_getfd()` requires `PTRACE_MODE_ATTACH` on the target process. Docker
containers run in separate PID namespaces. Without root, the kernel refuses
to copy the seccomp listener fd from the container process.

## Volumes

Mount host directories into containers:

```python
pg = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16",
    volumes = {"./pg-data": "/var/lib/postgresql/data"},
)
```

## What you learned

- `image=` pulls Docker images, `build=` builds from Dockerfile
- `.internal_addr` for container-to-container, `.addr` for host access
- `scenario(fn)` registers probes, `fault_assumption()` defines reusable faults
- `fault_scenario()` composes scenarios + faults + expectations
- Fault injection works identically on containers and binaries
- Syscall family expansion handles differences (pwrite64 vs write)
- Per-service filtering: only faulted services get seccomp overhead
- Requires Linux + Docker + sudo

**The key takeaway:** you can now test your actual production dependencies —
not mocks. "What happens when Postgres runs out of disk?" is tested against
real Postgres.

## What's next

You can test real infrastructure. But writing failure tests by hand is
slow — Chapter 10 shows how to auto-generate them.

**Continue:**
- [Chapter 10: Scenarios & Generation](10-scenarios.md) — register
  happy paths with `scenario()`, auto-generate fault mutations
- [Chapter 11: Event Sources & Observability](11-event-sources.md) — capture
  stdout, WAL changes, Kafka messages as trace events

**Reference:**
- [Spec Language Reference](../../spec-language.md) — complete API
- [CLI Reference](../../cli-reference.md) — all commands and flags
- Explore the `demo/faultbox.star` for a complete working example

## Exercises

1. **Read the trace**: Run postgres_write_failure with `--output trace.json`.
   Find the `pwrite64` events. What files was Postgres writing to? (Look at
   the `path` field.)

2. **Build your own service**: Create a directory with a Go HTTP server and
   Dockerfile. Declare it with `build="./my-svc"`. Does it build and start?

3. **Multiple faults**: Write a `fault_assumption` that faults BOTH Postgres
   (`write=deny("EIO")`) and create a second one for the API
   (`connect=delay("1s")`). Combine them in a `fault_scenario` with
   `faults=[pg_fault, api_fault]`. What happens when everything breaks?

4. **Graceful degradation**: The demo API connects to Redis but doesn't use it
   yet. Imagine it cached values in Redis. Write a `fault_assumption` that
   faults Redis (`connect=deny("ECONNREFUSED")`) and a `fault_scenario` that
   expects the API still works via Postgres. This tests graceful degradation —
   a key production resilience pattern.
