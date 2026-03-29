# PoC 2: Container Support

**Branch:** `poc/poc-2-containers`
**Status:** Complete
**Started:** 2026-03-28

## Context

PoC 1 proved the core engine with binary-mode fault injection. PoC 2 adds
container support so teams can test real infrastructure (Postgres, Redis, Kafka)
with the same seccomp-notify fault injection.

## Approach: Entrypoint Shim

A tiny static binary (`faultbox-shim`, ~3MB) is bind-mounted into Docker
containers, overriding the entrypoint. The shim installs the seccomp filter,
writes the listener fd to a shared file, then exec's the original entrypoint.

```
Host (faultbox)                          Container
┌──────────────────────┐    ┌──────────────────────────────────┐
│ Docker API:          │    │ /faultbox-shim (bind-mount, ro)  │
│  - Pull image        │    │   ├─ LockOSThread               │
│  - Create container  │    │   ├─ PR_SET_NO_NEW_PRIVS         │
│  - Override entry-   │    │   ├─ InstallFilter(syscallNrs)   │
│    point to shim     │    │   ├─ Write fd → /var/run/faultbox│
│  - Bind-mount shim   │    │   └─ exec(original entrypoint)   │
│  - Start container   │    │                                  │
│                      │    │ Target process runs with seccomp │
│ Read listener fd     │◄───│ filter → notifications sent to   │
│ via pidfd_getfd()    │    │ host via listener fd              │
│                      │    │                                  │
│ Notification loop    │    │ (same as PoC 1 binary mode)      │
│ (same as PoC 1)     │    │                                  │
└──────────────────────┘    └──────────────────────────────────┘
```

## Starlark API

```python
# Container from registry
postgres = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test"},
    healthcheck = tcp("localhost:5432"),
)

# Container from Dockerfile
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {"DATABASE_URL": "postgres://test@" + postgres.main.internal_addr + "/db"},
    depends_on = [postgres],
)

# Binary (existing, unchanged)
mock = service("mock", "/tmp/mock-svc", interface("main", "tcp", 9090))
```

### New attributes
- `service(..., image="postgres:16")` — pull and run container image
- `service(..., build="./api")` — build from Dockerfile
- `service(..., volumes={"./data": "/data"})` — volume mounts
- `.internal_addr` — `<container-name>:<port>` for container-to-container refs

## Implementation Steps

| Step | What | Status |
|------|------|--------|
| 1 | `cmd/faultbox-shim` — container entrypoint shim | **Done** (`cd2df90`) |
| 2 | Session external listener fd support | **Done** (`ad51264`) |
| 3 | `internal/container/` — Docker API package | **Done** (`ad51264`) |
| 4 | ServiceDef + Starlark changes (image=, build=) | **Done** (`ad51264`) |
| 5 | Runtime container lifecycle | **Done** (`ad51264`) |
| 6 | Demo: API + Postgres + Redis | **Done** |
| 7 | Kafka + extra service (stretch) | Deferred |

## Key Files

| File | Description |
|------|-------------|
| `cmd/faultbox-shim/main.go` | Container entrypoint shim (Linux-only) |
| `internal/container/docker.go` | Docker Engine API wrapper (pull, build, create, start, stop) |
| `internal/container/network.go` | Docker bridge network management |
| `internal/container/launch.go` | Container launch orchestration with shim injection |
| `internal/container/fd_linux.go` | `pidfd_getfd()` for acquiring seccomp listener fd |
| `internal/engine/launch_container_linux.go` | External listener notification loop |
| `internal/star/types.go` | ServiceDef: Image, Build, Volumes, IsContainer(), internal_addr |
| `internal/star/builtins.go` | service() with image=, build=, volumes= kwargs |
| `internal/star/runtime.go` | startContainerService(), buildContainerEnv(), cleanup |

## Network Model

- Binary mode: shared host network (no NET namespace)
- Container mode: Docker bridge network (`faultbox-net`)
- All containers on the same bridge — reach each other by container name
- Port mapping to host for healthchecks + test step execution
- `FAULTBOX_*` env vars use container hostnames for inter-container refs

## Demo

`poc/demo-container/` — Go API + Postgres + Redis

```bash
make demo-container    # run in Lima VM
```

Tests:
- `test_happy_path` — API health check passes
- `test_write_and_read` — Write to Postgres via API, read back
- `test_postgres_write_failure` — Deny write with EIO, expect 503
- `test_postgres_connect_failure` — Deny connect, expect unhealthy

## Dependencies

- Docker Engine in Lima VM (installed, tested)
- `github.com/docker/docker/client` Go package
- Kernel 5.6+ (same as PoC 1)
