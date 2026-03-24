# Faultbox Spec Language Reference

Faultbox uses two YAML files to define and test distributed systems:

- **`faultbox.yaml`** — Topology: what services exist and how they communicate
- **`spec.yaml`** — Specification: what scenarios to test and what to expect

Run with:

```bash
faultbox test --config faultbox.yaml --spec spec.yaml [--output results.json]
```

---

## Topology (`faultbox.yaml`)

The topology file describes the system under test: its services, their
communication interfaces, dependencies, health checks, and environment.

### Minimal Example

```yaml
version: "1"

services:
  db:
    binary: /usr/local/bin/my-db
    interfaces:
      main:
        protocol: tcp
        port: 5432
    healthcheck:
      test: tcp://localhost:5432
      timeout: 10s

  api:
    binary: /usr/local/bin/my-api
    interfaces:
      public:
        protocol: http
        port: 8080
    environment:
      PORT: "8080"
      DB_ADDR: "{{db.main.addr}}"
    depends_on: [db]
    healthcheck:
      test: http://localhost:8080/health
      timeout: 10s
```

### Multi-Interface Example

A service can expose multiple communication interfaces. Each interface has a
name, protocol, and port:

```yaml
services:
  courier:
    binary: ./courier-svc
    interfaces:
      public:
        protocol: http
        port: 8080
      internal:
        protocol: grpc
        port: 9090
      events:
        protocol: kafka
        port: 9092
        topics: [courier.updated, courier.assigned]
    depends_on: [db, cache]
    healthcheck:
      test: http://localhost:8080/health
      timeout: 15s
```

### Service Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `binary` | string | **yes** | Path to the executable |
| `args` | string[] | no | Command-line arguments |
| `interfaces` | map | **yes** | Named communication interfaces (see below) |
| `environment` | map | no | Environment variables (supports templates) |
| `depends_on` | string[] | no | Services that must start before this one |
| `healthcheck` | object | no | Readiness check configuration |

### Interface Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `protocol` | string | **yes** | Protocol type: `http`, `tcp`, `grpc`, `kafka`, etc. |
| `port` | int | **yes** | Port number the interface listens on |
| `topics` | string[] | no | Topic names (for async protocols like Kafka) |

Currently supported protocols for step execution: `http`, `tcp`.
Other protocols can be declared for documentation and future extension.

### Healthcheck Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `test` | string | — | Check URL: `tcp://host:port` or `http://host/path` |
| `interval` | duration | `1s` | Poll interval between checks |
| `timeout` | duration | `10s` | Overall timeout before marking service as unhealthy |

The health check runs after the service starts. If it doesn't pass within
`timeout`, the trace fails immediately with a reason explaining which service
wasn't ready.

### Environment Variables

#### User-Defined Variables

```yaml
environment:
  PORT: "8080"
  LOG_LEVEL: "debug"
```

#### Template Syntax

Reference other services' interfaces using `{{service.interface.field}}`:

```yaml
environment:
  DB_ADDR: "{{db.main.addr}}"      # → localhost:5432
  DB_HOST: "{{db.main.host}}"      # → localhost
  DB_PORT: "{{db.main.port}}"      # → 5432
  CACHE_ADDR: "{{cache.addr}}"     # shorthand when service has one interface
```

| Template | Resolves To |
|----------|-------------|
| `{{service.interface.addr}}` | `localhost:port` |
| `{{service.interface.host}}` | `localhost` |
| `{{service.interface.port}}` | Port number as string |
| `{{service.field}}` | Shorthand when service has a single interface |

#### Auto-Injected Variables

Faultbox automatically injects service discovery environment variables for
every service:

```
FAULTBOX_<SERVICE>_<INTERFACE>_ADDR=localhost:<port>
FAULTBOX_<SERVICE>_<INTERFACE>_HOST=localhost
FAULTBOX_<SERVICE>_<INTERFACE>_PORT=<port>
```

For example, if service `db` has interface `main` on port 5432, every service
receives:

```
FAULTBOX_DB_MAIN_ADDR=localhost:5432
FAULTBOX_DB_MAIN_HOST=localhost
FAULTBOX_DB_MAIN_PORT=5432
```

User-defined variables override auto-injected ones if the same key is used.

### Legacy Format

The previous topology format is still accepted and auto-migrated:

```yaml
# Legacy format (still works)
services:
  db:
    binary: ./my-db
    port: 5432               # → interfaces.default.port
    env:                      # → environment
      PORT: "5432"
    ready: tcp://localhost:5432  # → healthcheck.test
```

| Legacy Field | Migrates To |
|-------------|-------------|
| `port: N` | `interfaces: { default: { protocol: tcp, port: N } }` |
| `env:` | `environment:` |
| `ready: "check"` | `healthcheck: { test: "check", timeout: 10s }` |

---

## Specification (`spec.yaml`)

The spec file defines test traces — scenarios that exercise the system with
specific faults and verify expected behavior.

### Minimal Example

```yaml
version: "1"
system: faultbox.yaml

traces:
  happy-path:
    description: "Normal operation — all services healthy"
    faults: {}
    timeout: "15s"
    steps:
      - api.get:
          path: /health
          expect:
            status: 200
    assert:
      - exit_code: { service: api, equals: 0 }
```

### Full Example

```yaml
version: "1"
system: faultbox.yaml

traces:
  happy-path:
    description: "Normal operation"
    faults: {}
    timeout: "15s"
    steps:
      - db.main.send:
          data: "PING"
          expect:
            equals: "PONG"
      - api.public.post:
          path: /data/key1
          body: "hello"
          expect:
            status: 200
            body_contains: "stored"
      - api.public.get:
          path: /data/key1
          expect:
            status: 200
            body_equals: "hello"
    assert:
      - exit_code: { service: api, equals: 0 }

  db-slow-writes:
    description: "DB writes are delayed 500ms"
    faults:
      db:
        - syscall: write
          action: delay
          delay: 500ms
          probability: "100%"
    timeout: "15s"
    steps:
      - api.public.post:
          path: /data/key1
          body: "value1"
          expect:
            status: 200
    assert:
      - exit_code: { service: api, equals: 0 }

  api-cannot-reach-db:
    description: "API's connections to DB are refused"
    faults:
      api:
        - "connect=ECONNREFUSED:100%"
    timeout: "15s"
    steps:
      - api.public.post:
          path: /data/key1
          body: "value1"
          expect:
            status: 500
            body_contains: "db error"
    assert:
      - exit_code: { service: api, equals: 0 }
```

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `version` | string | no | Spec version (currently `"1"`) |
| `system` | string | no | Reference to topology file (documentation only) |
| `traces` | map | **yes** | Named test traces |

### Trace Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `description` | string | no | Human-readable description of the scenario |
| `faults` | map | no | Per-service fault rules (see Faults below) |
| `steps` | list | no | Ordered actions to exercise the system (see Steps below) |
| `assert` | list | no | Post-execution assertions (see Assertions below) |
| `timeout` | duration | no | Trace timeout (default: `30s`) |

### Execution Order

For each trace, Faultbox executes in this order:

```
1. Start all services (in dependency order)
2. Wait for health checks to pass
3. Run steps sequentially
4. Run eventually assertions (while services are alive)
5. Stop all services (SIGTERM → SIGKILL after 2s)
6. Check exit code assertions
7. Report results
```

Services are fully restarted between traces to ensure clean state.

---

## Faults

Faults are per-service and injected at the syscall level via seccomp-notify.
Each service can have zero or more fault rules.

### String Format

Compact syntax compatible with the `--fault` CLI flag:

```yaml
faults:
  api:
    - "connect=ECONNREFUSED:100%"              # deny
    - "write=delay:200ms:50%"                   # delay
    - "openat=ENOENT:100%:/data/*:after=2"      # deny + path + trigger
```

See the [CLI Reference](cli-reference.md) for full string syntax documentation.

### Object Format

Structured syntax for readability and LLM generation:

```yaml
faults:
  db:
    - syscall: write
      action: delay
      delay: 500ms
      probability: "100%"

    - syscall: connect
      action: deny
      errno: ECONNREFUSED
      probability: "10%"
      trigger:
        type: after
        n: 2
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `syscall` | string | **yes** | Linux syscall name (`write`, `connect`, `openat`, etc.) |
| `action` | string | **yes** | `deny` (return errno) or `delay` (sleep then allow) |
| `errno` | string | for deny | Error code: `EIO`, `ECONNREFUSED`, etc. (default: `EIO`) |
| `delay` | duration | for delay | Sleep duration: `500ms`, `2s`, etc. |
| `probability` | string | **yes** | Fire probability: `"100%"`, `"50%"`, `"1%"` |
| `path` | string | no | Glob pattern for file syscalls: `/data/*` |
| `trigger` | object | no | Stateful trigger (see below) |

### Trigger Object

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | `nth` (fire on Nth call) or `after` (fire after N calls) |
| `n` | int | Count parameter (1-indexed for `nth`, 0-indexed for `after`) |

### Mixing Formats

String and object formats can be mixed in the same fault list:

```yaml
faults:
  api:
    - "connect=ECONNREFUSED:100%"          # string
    - syscall: write                        # object
      action: delay
      delay: 200ms
      probability: "50%"
```

### Important: Fault Targeting

Faults are applied to the **service's own syscalls**, not to syscalls made by
other services connecting to it. For example:

```yaml
# CORRECT: fault API's outbound connect() — API can't reach DB
faults:
  api:
    - "connect=ECONNREFUSED:100%"

# WRONG: this faults DB's own connect() calls (DB doesn't make any)
faults:
  db:
    - "connect=ECONNREFUSED:100%"
```

To simulate "DB is slow to respond", fault DB's `write` syscall (which delays
the response):

```yaml
faults:
  db:
    - syscall: write
      action: delay
      delay: 500ms
      probability: "100%"
```

---

## Steps

Steps are the active scenario driver — they exercise the system by sending
requests and validating responses. Steps run sequentially after all services
are ready.

### Step Addressing

Steps use `service[.interface].operation` syntax:

```yaml
steps:
  - api.get:                   # service "api", default interface, HTTP GET
      path: /health
  - api.public.post:           # service "api", interface "public", HTTP POST
      path: /data/key1
  - db.main.send:              # service "db", interface "main", TCP send
      data: "PING"
```

When a service has a **single interface**, the interface name can be omitted:
`api.get` resolves to `api.public.get` if `public` is the only interface.

The operation maps to the interface's protocol:
- `http` interface → `get`, `post`, `put`, `delete`, `patch`
- `tcp` interface → `send`

### HTTP Steps

Available when the target interface has `protocol: http`.

```yaml
- api.get:
    path: /health
    headers:
      Authorization: "Bearer token123"
    expect:
      status: 200
      body_contains: "ok"

- api.post:
    path: /data/key1
    body: "hello world"
    expect:
      status: 200
      body_equals: "stored: key1=hello world"
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | string | no | URL path (default: `/`) |
| `body` | string | no | Request body (for POST/PUT/PATCH) |
| `headers` | map | no | HTTP headers |
| `expect` | object | no | Response expectations (see below) |

**HTTP operations:** `get`, `post`, `put`, `delete`, `patch`

**HTTP expect fields:**

| Field | Type | Description |
|-------|------|-------------|
| `status` | int | Expected HTTP status code (e.g., `200`, `500`) |
| `body_contains` | string | Response body must contain this substring |
| `body_equals` | string | Response body must exactly match this string |

### TCP Steps

Available when the target interface has `protocol: tcp`.

```yaml
- db.main.send:
    data: "PING"
    expect:
      equals: "PONG"

- db.main.send:
    data: "SET mykey myvalue"
    expect:
      contains: "OK"
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `data` | string | **yes** | Data to send (newline is appended automatically) |
| `expect` | object | no | Response expectations |

**TCP expect fields:**

| Field | Type | Description |
|-------|------|-------------|
| `contains` | string | Response line must contain this substring |
| `equals` | string | Response line must exactly match this string |

TCP steps open a new connection, send one line, read one line response, and
close. They are designed for request-response text protocols.

### Sleep Step

Pause execution for a duration:

```yaml
- sleep: 1s
- sleep: 500ms
- sleep: 100ms
```

### Step Failure

If any step fails (unexpected status, body mismatch, connection error), the
trace stops immediately and reports the failure. Subsequent steps are skipped.

A step failure means the **trace** fails — this is separate from assertions
which run after steps complete.

---

## Assertions

Assertions verify system behavior after steps have run and services have been
stopped.

### Exit Code Assertion

Check that a service exited with a specific code:

```yaml
assert:
  - exit_code:
      service: api
      equals: 0
```

Services that were still running when the trace ended (killed by Faultbox) are
considered to have exit code `0` — they didn't crash, we intentionally stopped
them.

### Eventually Assertion

Poll an HTTP endpoint until it returns the expected status:

```yaml
assert:
  - eventually:
      timeout: 5s
      http:
        url: http://localhost:8080/ready
        status: 200
```

Eventually assertions run **while services are still alive** (before shutdown).
They retry with 200ms intervals until the timeout expires.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `timeout` | duration | `5s` | Maximum time to wait |
| `http.url` | string | — | Full URL to poll |
| `http.status` | int | — | Expected HTTP status code |

---

## Results (`results.json`)

When `--output` is specified, Faultbox writes structured results:

```json
{
  "simulation_id": "5785c75f752a78bc",
  "duration_ms": 1937,
  "traces": [
    {
      "name": "happy-path",
      "result": "pass",
      "duration_ms": 216,
      "services": {
        "api": { "exit_code": 0, "faults_applied": 0 },
        "db": { "exit_code": 0, "faults_applied": 0 }
      },
      "steps": [
        {
          "action": "HTTP GET /health",
          "success": true,
          "status_code": 200,
          "body": "ok",
          "duration_ms": 2
        }
      ]
    },
    {
      "name": "db-timeout",
      "result": "fail",
      "reason": "step \"HTTP POST /data/key1\" failed: expected status 200, got 500",
      "duration_ms": 5012,
      "services": { ... },
      "steps": [ ... ]
    }
  ],
  "pass": 1,
  "fail": 1
}
```

### Result Fields

| Field | Type | Description |
|-------|------|-------------|
| `simulation_id` | string | Unique run identifier |
| `duration_ms` | int | Total simulation time in milliseconds |
| `traces[]` | array | Per-trace results |
| `pass` | int | Number of passing traces |
| `fail` | int | Number of failing traces |

### Trace Result Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Trace name (from spec) |
| `result` | string | `"pass"` or `"fail"` |
| `reason` | string | Failure reason (only when `result` is `"fail"`) |
| `duration_ms` | int | Trace execution time |
| `services` | map | Per-service exit codes and fault counts |
| `steps` | array | Per-step results with timing |

---

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | All traces passed |
| 1 | Faultbox error (bad config, failed to start, etc.) |
| 2 | One or more traces failed |

---

## Protocol Extensibility (Roadmap)

The step executor is designed around a protocol layering model:

| Layer | Examples | Status |
|-------|----------|--------|
| **L4 Core** | `tcp`, `udp` | `tcp` built-in |
| **L7 Stdlib** | `http`, `grpc` | `http` built-in |
| **L7 Extensions** | `postgres`, `kafka`, `redis` | Future: Starlark modules |

Currently `http` and `tcp` are built-in Go implementations. Future versions
will support Starlark-based protocol modules that implement the same
`StepHandler` interface, allowing community-defined protocols without changes
to the spec YAML format.

```python
# Example future Starlark module: redis.star
def set(ctx, key, value):
    resp = ctx.tcp.send(
        addr = ctx.service("cache").addr,
        data = f"SET {key} {value}\r\n",
    )
    assert.eq(resp.line(), "+OK")
```

Usage in spec (no YAML changes needed):
```yaml
steps:
  - cache.set: { key: "session:123", value: "active" }
```
