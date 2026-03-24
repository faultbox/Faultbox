# PoC Step 6: Multi-Service Architecture

**Branch:** `poc/step-6-multi-service`
**Status:** Complete
**Date:** 2026-03-24

## Context

Steps 1-5 built a complete single-service fault injection engine. Step 6 is
the **inflection point** — shifting from "control one process" to "control a
distributed system." This is a design step: we make architectural decisions
before writing code.

### Target Users

Faultbox serves two equally important user personas:

1. **Human engineers** (Engineer/HoE/CTO) — investigate software behavior,
   make distributed systems safer, understand failure modes
2. **LLM agents** (Claude, Codex, etc.) — verify their own code changes
   autonomously in a code → test → fix loop

Both are **first-class citizens.** Every API, CLI flag, config format, and
output format must work for both:
- Humans need readable logs and clear CLI help
- Agents need structured JSON output, deterministic exit codes, machine-parseable
  results, and zero interactive prompts

The full agent loop:
```
1. Agent writes/modifies code
2. Agent generates faultbox.yaml + spec.yaml (LLM-authored specs)
3. Agent runs: faultbox run --config f.yaml --spec spec.yaml --output results.json
4. Agent reads results.json → pass/fail per trace
5. If failures → agent reads failing trace details, fixes code, goto 1
```

This is `go test -json` for distributed systems correctness.

### What We Have (Steps 1-5)

```
faultbox run [flags] <binary>
```

One binary, one isolated namespace, one seccomp filter, one notification loop.
Capabilities: deny, delay, path filtering, system path exclusion, stateful
triggers (nth/after), env passthrough.

### What We Need

```
faultbox run --config system.yaml
```

Multiple binaries, each isolated, each independently faulted, able to
communicate with each other, orchestrated by a single Faultbox process.

---

## Design Decisions

### 1. Topology Manifest Format

**Proposal:** YAML file describing services, their configuration, and fault rules.

```yaml
# faultbox.yaml — topology only, no faults
version: "1"

services:
  api:
    binary: ./bin/api-server
    args: ["--port", "8080"]
    port: 8080
    env:
      DB_URL: "postgres://localhost:5432/myapp"
      CACHE_URL: "redis://localhost:6379"
    depends_on: [db]
    ready: http://localhost:8080/health

  worker:
    binary: ./bin/worker
    args: ["--queue-url", "amqp://localhost:5672"]
    env:
      DB_URL: "postgres://localhost:5432/myapp"
    depends_on: [db]

  db:
    binary: ./bin/mock-postgres
    port: 5432
    env:
      DATA_DIR: "/data/pgdata"
    ready: tcp://localhost:5432
```

Faults are NOT in the topology file — they live in `spec.yaml` (the test plan).
This separation allows experimenting with different failure scenarios against
the same service topology.

```yaml
# spec.yaml — traces with faults and expectations
version: "1"
system: faultbox.yaml

traces:
  happy-path:
    description: "Normal operation — all services healthy"
    faults: {}
    expect:
      api: {exit_code: 0}

  db-write-failure:
    description: "DB writes fail — API should handle gracefully"
    faults:
      db:
        - "write=EIO:100%:after=2"
    expect:
      api: {exit_code: 0}

  slow-db:
    description: "DB latency spike — API should timeout"
    faults:
      db:
        - "read=delay:5s:100%"
    expect:
      api: {exit_code: 0}
      timeout: 10s
```

**Key design choices:**

- **Topology is stable, faults are dynamic** — one `faultbox.yaml`, many `spec.yaml` files
- **Service names resolve to localhost ports** inside the shared network
- **Each service = one Session** with its own PID/MNT/USER namespace + seccomp filter
- **Faults are per-trace, per-service** in the spec file
- **LLM-friendly:** flat YAML, no special syntax, clear field names

**Discussion point:** Should `binary` support container images (`image: postgres:16`)
in addition to local binaries? This would let users run real Postgres instead of
mocks. For the PoC, local binaries only.

---

### 2. Inter-Service Networking

**The problem:** Each service is in its own NET namespace (isolated network stack).
They can't talk to each other by default.

**Three options:**

#### Option A: Shared Network Namespace
All services share one NET namespace. Each gets its own PID/MNT/USER namespace
but sees the same network stack.

```
Pros: Trivial — services bind to different ports, localhost works
Cons: No per-service network faults (tc/netem affects all), no network isolation
```

#### Option B: Veth Pairs + Bridge
Each service gets a veth pair connecting it to a Faultbox-managed bridge.
Services get unique IPs (10.0.0.1, 10.0.0.2, etc.).

```
Pros: Full isolation, per-service network faults possible
Cons: Requires CAP_NET_ADMIN on host, complex setup, DNS needed
```

#### Option C: Shared Network + Seccomp for Network Faults
Services share one NET namespace (Option A for connectivity), but Faultbox uses
seccomp to intercept connect/send/recv per-service for fault injection.

```
Pros: Simple connectivity, per-service faults via existing seccomp mechanism
Cons: No packet-level faults (but we decided those are low priority)
```

**My recommendation: Option C** for the PoC. Rationale:
- We already have seccomp-based network faults (deny, delay) from Steps 2+4
- Shared network namespace means services just use `localhost:PORT`
- No CAP_NET_ADMIN needed (stays unprivileged)
- Per-service faults work because each service has its own seccomp filter
- For Step 7 (message ordering), we intercept sendto/recvfrom per-service anyway

The manifest resolves service names to ports:

```yaml
services:
  api:
    binary: ./bin/api-server
    port: 8080          # Faultbox assigns, sets PORT env var
  db:
    binary: ./bin/mock-postgres
    port: 5432
```

Inside the shared network, `api` connects to `localhost:5432` to reach `db`.
Faultbox sets `DB_URL=postgres://localhost:5432/myapp` automatically based on
service name resolution.

---

### 3. Service Lifecycle

**Start order:** Services start in dependency order (or parallel if no deps).

```yaml
services:
  db:
    binary: ./bin/mock-postgres
    ready: tcp://localhost:5432    # health check
  api:
    binary: ./bin/api-server
    depends_on: [db]              # wait for db.ready before starting
    ready: http://localhost:8080/health
  worker:
    binary: ./bin/worker
    depends_on: [db]
```

**Lifecycle:**
1. Parse manifest, resolve dependency order
2. Start services in order, wait for `ready` check before proceeding
3. All services running → run test / apply spec traces
4. On completion (or Ctrl+C): signal all services, wait for exit, collect results

**Ready checks:**
- `tcp://host:port` — TCP connect succeeds
- `http://host:port/path` — HTTP 200
- `exec:command` — command exits 0
- Timeout: configurable, default 10s

---

### 4. Architecture: Engine Changes

Current:
```
Engine
  └── Session (1 binary, 1 namespace, 1 seccomp filter)
```

Proposed:
```
Engine
  └── Simulation
        ├── Service "api"    → Session (binary, namespace, seccomp)
        ├── Service "worker" → Session (binary, namespace, seccomp)
        └── Service "db"     → Session (binary, namespace, seccomp)
```

New types:

```go
// Simulation orchestrates multiple services running together.
type Simulation struct {
    ID       string
    Config   SimulationConfig
    Services map[string]*Service
    Network  *NetworkConfig  // shared network namespace
}

// SimulationConfig is parsed from faultbox.yaml.
type SimulationConfig struct {
    Version  string
    Services map[string]ServiceConfig
}

// ServiceConfig describes one service in the topology (no faults — those live in spec.yaml).
type ServiceConfig struct {
    Binary    string
    Args      []string
    Env       map[string]string
    Port      int
    DependsOn []string
    Ready     string            // health check URL/command
}

// TraceConfig describes one test trace from spec.yaml.
type TraceConfig struct {
    Description string
    Faults      map[string][]string  // service name → fault rule specs
    Expect      map[string]TraceExpect
    Timeout     time.Duration
}

// TraceExpect describes expected outcomes for a service in a trace.
type TraceExpect struct {
    ExitCode *int  // nil = don't check
}

// Service wraps a Session with multi-service context.
type Service struct {
    Name    string
    Config  ServiceConfig
    Session *Session
    State   ServiceState        // pending, starting, ready, stopped, failed
}
```

**The Session stays unchanged.** A Simulation creates multiple Sessions, each
with its own SessionConfig. The Simulation handles:
- Dependency ordering
- Health checks
- Coordinated shutdown
- Aggregated results

---

### 5. CLI Changes

```bash
# Single service (unchanged)
faultbox run [flags] <binary> [args...]

# Multi-service: execute spec traces, report results, exit
faultbox test --config faultbox.yaml --spec spec.yaml [--output results.json]
```

For the PoC, only `faultbox test` is implemented for multi-service. Interactive
mode (`faultbox run --config` with live spec injection) is post-PoC.

**`faultbox test` flow:**
1. Parse `faultbox.yaml` (topology) and `spec.yaml` (traces)
2. For each trace:
   a. Start all services in dependency order, wait for `ready` checks
   b. Apply per-service faults from the trace
   c. Wait for services to exit (or timeout)
   d. Check assertions (exit codes, eventually checks)
   e. Stop all services, collect results
   f. **Restart clean** for next trace (seccomp filters can't be removed)
3. Write results to `--output` (if specified) and exit with code 0/1/2

**Why restart per trace:** Seccomp filters persist until process exit and
stateful trigger counters accumulate. Each trace needs a clean slate.

---

### 6. Networking Detail: Host Network

For multi-service mode, **skip NET namespace entirely.** All services share
the host's network stack and bind to different ports on localhost. Isolation
comes from PID/MNT/USER namespaces only.

```go
// Multi-service: no CLONE_NEWNET
svc.Cloneflags = CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWUSER
```

Network faults (delay, deny on connect/send/recv) still work via per-service
seccomp filters. Each service has its own notification loop.

This is sufficient for the PoC. Shared NET namespace via `setns()` can be
added later if true network isolation between services is needed.

---

### 7. Deployment Considerations (Future)

Not implementing in Step 6, but designing with awareness:

| Target | How Faultbox Runs | What Changes |
|---|---|---|
| **Local** | `faultbox run --config f.yaml` | Current approach |
| **CI/CD** | `docker run faultbox --config f.yaml` | Container image with Faultbox binary |
| **K8s** | Sidecar or operator | Needs seccomp profile on pod, ephemeral container support |

**K8s sidecar pattern:**
- Faultbox runs as a sidecar container in the same pod
- Uses `shareProcessNamespace: true` to access target's PID
- Installs seccomp filter via container security context
- No namespace management needed (k8s handles that)

**Key constraint:** k8s seccomp profiles are pod-level, not container-level.
Faultbox would need a custom seccomp profile that routes notifications to the
sidecar. This is advanced but doable (Kubernetes 1.19+ supports custom seccomp).

**CI/CD is the most likely first deployment target** — an LLM agent running in
a GitHub Actions workflow uses `faultbox run --config f.yaml --output results.json`
to verify code changes before merge. This requires only a container image.

---

## Implementation Plan for Step 6

1. **Add `gopkg.in/yaml.v3` dependency**
2. **Parse `faultbox.yaml`** — `SimulationConfig` struct, YAML loader (topology only)
3. **Parse `spec.yaml`** — `SpecConfig` struct with traces, per-service faults, expectations
4. **Simulation orchestrator** — start services in dependency order with health checks,
   restart clean between traces, coordinated shutdown
5. **Host network** — skip NET namespace for multi-service, PID/MNT/USER only
6. **CLI: `faultbox test --config --spec`** — execute traces, report pass/fail
7. **Structured results** — `--output results.json` for agent consumption
8. **Multi-service log output** — `service` field in JSON logs, `[name]` prefix in console
9. **Build mock services** — `poc/mock-api/` and `poc/mock-db/` for 2-service testing
10. **Test with 2-service example** — mock API calls mock DB over localhost

Interactive mode (`faultbox run --config` with live spec injection) is post-PoC.

---

### 8. Output and Results (Agent-Ready)

**Exit codes** — machine contract:

| Code | Meaning |
|---|---|
| 0 | All services exited cleanly / all traces passed |
| 1 | Faultbox error (bad config, failed to start) |
| 2 | Some traces failed (expected behavior not observed) |

**Structured results** via `--output results.json`:

```json
{
  "simulation_id": "a1b2c3",
  "duration_ms": 5230,
  "services": {
    "api": {"exit_code": 0, "faults_injected": 12},
    "worker": {"exit_code": 1, "faults_injected": 3}
  },
  "traces": [
    {"name": "happy-path", "result": "pass", "duration_ms": 1200},
    {"name": "fsync-failure", "result": "fail", "reason": "client received ack after fsync EIO", "duration_ms": 800}
  ],
  "pass": 3,
  "fail": 1
}
```

**Log output:**
- `--log-format=json` (default when piped / used by agent): structured JSON lines
  with `service` field identifying which service produced the event
- `--log-format=console` (for humans): lines prefixed with `[service-name]` in color

```json
{"time":"...","level":"INFO","service":"api","msg":"syscall intercepted","name":"connect","decision":"delay(200ms)"}
{"time":"...","level":"INFO","service":"worker","msg":"syscall intercepted","name":"write","decision":"deny(EIO)"}
```

---

## Resolved Decisions

1. **Networking:** Host network (no NET namespace for multi-service).
   Network faults via per-service seccomp filters.
2. **Readiness:** TCP/HTTP health checks — deterministic, required for agents.
3. **Container images:** Binary-only for PoC. Image support is post-PoC.
4. **Service discovery:** Env var injection (`DB_HOST=localhost`, `DB_PORT=5432`).
5. **Topology vs faults:** `faultbox.yaml` is topology only (stable).
   `spec.yaml` has faults per trace (dynamic, experimentation).
6. **Invariant checking:** Outside PoC scope. Trace-first is the foundation;
   invariant exploration can generate traces later without engine changes.
7. **Restart per trace:** Services are restarted between traces for clean state
   (seccomp filters can't be removed, trigger counters accumulate).
8. **Interactive mode:** Post-PoC. Only `faultbox test` (CI/CD batch mode) for now.
9. **YAML dependency:** `gopkg.in/yaml.v3` for config parsing.

## Resolved: Assertion Language

### Vision (Post-PoC)

Expressive DSL powered by **Starlark** (Google's sandboxed Python subset) with
temporal operators and a lambda runtime:

```yaml
assert:
  # Temporal: "eventually Redis has this key"
  - eventually(timeout: 10s):
      redis.get("localhost:6379", "order:1") != None

  # Safety: "client never sees ack after failure"
  - never(during: 3s):
      "ack" in service("api").stdout()

  # Lambda: custom check logic
  - check:
      name: "exactly 3 retries"
      run: |
        logs = service("worker").stdout()
        retries = [l for l in logs if "retry" in l]
        assert len(retries) == 3
```

Three temporal operators (from LTL, simplified):

| Operator | Meaning | Use case |
|---|---|---|
| `eventually(timeout)` | Must become true within timeout | Eventual consistency, recovery |
| `always(during)` | Must stay true for entire duration | Safety, no-regression |
| `never(during)` | Must never become true during period | "No ack after failure" |

Built-in functions: `http.get/post`, `tcp.connect`, `redis.get`, `pg.query`,
`service(name).stdout/stderr/logs`, `file.read`. All sandboxed with timeouts.

This IS ADR-021 Layer 4 (Monitors). Same assertions work against simulation
(testing) and production (monitoring) in the future.

### PoC Scope (Steps 6-8)

For the PoC demo, assertions are limited to:

- **`exit_code`** — check process exit code
- **`eventually(http.get)`** — poll an HTTP endpoint until expected status

```yaml
assert:
  - api.exit_code == 0
  - eventually(timeout: 5s):
      http.get("http://localhost:8080/health").status == 200
```

This covers 80% of real scenarios. Starlark runtime, temporal operators, and
lambda functions grow incrementally in post-PoC steps.

---

## Connection to Roadmap

### PoC Steps (6-10)

```
Step 6 (this) → multi-service topology + simulation orchestrator
Step 7  → spec language: YAML traces + assertions (exit_code + eventually)
Step 8  → trace executor: run specs against running services, report results
Step 9  → LLM integration: prompt templates, code-to-spec generation
Step 10 → deployment: container image for CI/CD
```

### Post-PoC Vision

```
Starlark assertion runtime → temporal operators (always, never, eventually)
Lambda checks              → redis.get, pg.query, service().stdout
Invariant explorer         → auto-generate traces from high-level properties
K8s operator               → sidecar pattern, seccomp profiles
Production monitors        → same assertions run against live systems (ADR-021 L4)
```
