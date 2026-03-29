# Chapter 7: Containers — Real Infrastructure

Previous chapters used mock binaries. In this chapter you'll test real
Postgres, Redis, and a Go API — all in Docker containers, with fault injection.

## Prerequisites

- Docker daemon running
- Linux kernel 5.6+ (Lima VM on macOS)
- `sudo` access (required for `pidfd_getfd` across Docker namespaces)

## How container mode works

```
Host (faultbox)                          Docker Container
┌─────────────────────┐    ┌────────────────────────────────┐
│ 1. Pull/build image │    │ faultbox-shim (bind-mounted)   │
│ 2. Override entry-  │    │   ├─ Install seccomp filter    │
│    point with shim  │    │   ├─ Report listener fd        │
│ 3. Acquire fd via   │◄───│   ├─ Wait for host ACK         │
│    pidfd_getfd()    │    │   └─ exec(original entrypoint) │
│ 4. Run notification │    │                                │
│    loop (same as    │    │ Real service runs with         │
│    binary mode)     │    │ seccomp filter active           │
└─────────────────────┘    └────────────────────────────────┘
```

The key: a tiny shim binary is injected into the container. It installs the
seccomp filter, then exec's the original entrypoint. From the service's
perspective, nothing changed — except faultbox can now intercept its syscalls.

## Container services

Instead of `binary=`, use `image=` or `build=`:

```python
# Pull from registry
postgres = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432", timeout="60s"),
)

# Build from Dockerfile
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {"PORT": "8080", "DATABASE_URL": "postgres://postgres:test@" + postgres.main.internal_addr + "/testdb?sslmode=disable"},
    depends_on = [postgres],
    healthcheck = http("localhost:8080/health", timeout="60s"),
)
```

Three service sources — exactly one required:

| Parameter | Source |
|-----------|--------|
| `"/path/to/binary"` (positional) | Local binary (PoC 1) |
| `image = "postgres:16"` | Docker image from registry |
| `build = "./api"` | Build from Dockerfile directory |

## Container networking

Containers run on a Docker bridge network (`faultbox-net`). They reach each
other by service name:

```python
# internal_addr returns "postgres:5432" (container hostname)
env = {"DATABASE_URL": "postgres://test@" + postgres.main.internal_addr + "/db"}

# addr returns "localhost:32847" (host-mapped random port)
# Used by: healthchecks, test step execution (api.get(...))
```

| Attribute | Returns | Used by |
|-----------|---------|---------|
| `.internal_addr` | `"postgres:5432"` | Container-to-container env vars |
| `.addr` | `"localhost:32847"` | Faultbox healthchecks and test steps |

## The demo

The container demo is at `poc/demo-container/`. Run it:

```bash
# Build binaries + demo API image
make demo-build

# Run in Lima VM with Docker
sudo bin/linux-arm64/faultbox test poc/demo-container/faultbox.star
```

It starts Postgres, Redis, and a Go API, then runs 4 tests:

```
--- PASS: test_happy_path (9.7s) ---
--- PASS: test_postgres_write_enospc (10.4s) ---
--- PASS: test_postgres_write_failure (9.4s) ---
--- PASS: test_write_and_read (9.7s) ---
```

## Fault injection on containers

Same `fault()` API — Faultbox doesn't care if it's a binary or container:

```python
def test_postgres_disk_full():
    """Postgres disk full — API should return 503."""
    def scenario():
        resp = api.post(path="/data?key=full&value=test")
        assert_true(resp.status >= 500, "expected 5xx on ENOSPC")
    fault(postgres, write=deny("ENOSPC"), run=scenario)
```

This denies `write`, `writev`, and `pwrite64` on the Postgres container.
When Postgres tries to write a data page, it gets ENOSPC. The SQL query
fails, the API returns 503.

## Per-service filtering

Faultbox only installs seccomp filters on services that are actually faulted.
In the demo, only Postgres gets a filter — Redis and API run at native speed.

This is why fault tests and non-fault tests have similar timing.

## Volumes

Mount host directories into containers:

```python
pg = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    volumes = {"./pg-data": "/var/lib/postgresql/data"},
)
```

## Why sudo?

`pidfd_getfd()` requires `PTRACE_MODE_ATTACH` permission on the target process.
Docker containers run in separate PID namespaces. Without root, the kernel
refuses to copy the seccomp listener fd from the container process.

Future: this could be solved with a setuid helper or Docker plugin.

## What you learned

- `image=` pulls Docker images, `build=` builds from Dockerfile
- `.internal_addr` for container-to-container references
- `.addr` for host-side access (healthchecks, test steps)
- Fault injection works identically on containers and binaries
- Per-service filtering: only faulted services get seccomp overhead
- Requires Linux + Docker + sudo

## Exercises

1. **Graceful degradation**: The demo API talks to both Postgres and Redis.
   Currently Redis is unused. Modify `poc/demo-container/api/main.go` to
   cache values in Redis. Then write a test that faults Redis with
   `connect=deny("ECONNREFUSED")` and verifies the API still works
   (falls back to Postgres).

2. **Read the trace**: Run the write_failure test with `--output trace.json`.
   Open the JSON and find the `pwrite64` syscall events with
   `decision: "deny(input/output error)"`. What files was Postgres trying
   to write to? (Look at the `path` field.)

3. **Build your own**: Create a new directory with a simple Go HTTP server
   and Dockerfile. Declare it with `build="./my-svc"` in a .star file.
   Verify it builds and starts correctly.

4. **Multiple faults**: Write a test that faults BOTH Postgres (write=EIO)
   and the API (connect=delay("1s")). What happens when everything is broken
   at once?
