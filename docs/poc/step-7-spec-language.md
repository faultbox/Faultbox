# Step 7: Spec Language Design

## Goal

Formalize the Faultbox spec language with:
- Multi-interface service topology with auto-injected service discovery
- Active scenario driver (`steps`) using `service.interface.operation` addressing
- Structured fault DSL (object form alongside existing string form)
- Compose-compatible field naming

## Design Decisions

### 1. Services expose named interfaces

A service can have multiple communication interfaces (public HTTP, internal gRPC,
async Kafka topics). Each interface has a protocol, port, and optional metadata.

```yaml
# faultbox.yaml
version: "1"

services:
  db:
    binary: /tmp/mock-db
    interfaces:
      main:
        protocol: tcp
        port: 5432
    depends_on: []
    healthcheck:
      test: tcp://localhost:5432
      interval: 1s
      timeout: 10s

  api:
    binary: /tmp/mock-api
    interfaces:
      public:
        protocol: http
        port: 8080
    depends_on: [db]
    healthcheck:
      test: http://localhost:8080/health
      interval: 1s
      timeout: 10s
```

Real-world example (not implemented in PoC, shows the model):
```yaml
  courier:
    binary: ./courier
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
```

Services with a single interface allow shorthand in steps:
`api.post` resolves to `api.public.post` when `public` is the only interface.

### 2. Auto-injected service discovery

Faultbox auto-injects env vars so services can find each other without hardcoded
addresses. For each dependency, the following env vars are set:

```
FAULTBOX_<SERVICE>_<INTERFACE>_ADDR=host:port
FAULTBOX_<SERVICE>_<INTERFACE>_HOST=host
FAULTBOX_<SERVICE>_<INTERFACE>_PORT=port
```

Example: `api` depends on `db` →
```
FAULTBOX_DB_MAIN_ADDR=localhost:5432
FAULTBOX_DB_MAIN_HOST=localhost
FAULTBOX_DB_MAIN_PORT=5432
```

Additionally, topology env vars support template syntax:
```yaml
services:
  api:
    environment:
      DB_ADDR: "{{db.main.addr}}"    # resolved to localhost:5432
```

### 3. Steps — the active scenario driver

Specs now include `steps` that actively exercise the system. Steps run
sequentially after all services are ready, before final assertions.

```yaml
traces:
  db-timeout-on-write:
    description: "DB write timeout — API returns 500"
    faults:
      db:
        - { syscall: write, action: delay, delay: 5s, probability: 100% }
    steps:
      - api.post:
          path: /data/key1
          body: "value1"
          expect:
            status: 500
            body_contains: "timeout"
      - api.get:
          path: /health
          expect:
            status: 200
      - db.main.send:
          data: "PING\n"
          expect:
            contains: "PONG"
      - sleep: 1s
    assert:
      - exit_code: { service: api, equals: 0 }
```

Step addressing: `service[.interface].operation`

### 4. Built-in step types (PoC)

**HTTP protocol** (resolved when interface protocol = http):
- `service.get`:  `{ path, headers?, expect? }`
- `service.post`: `{ path, body?, headers?, expect? }`
- `service.put`:  `{ path, body?, headers?, expect? }`
- `service.delete`: `{ path, expect? }`

HTTP expect fields:
- `status`: expected HTTP status code
- `body_contains`: substring match on response body
- `body_equals`: exact match on response body

**TCP protocol** (resolved when interface protocol = tcp):
- `service.send`: `{ data, expect? }`

TCP expect fields:
- `contains`: substring match on response line
- `equals`: exact match on response line

**Utility:**
- `sleep`: duration string (e.g., `1s`, `500ms`)

### 5. Structured fault syntax

Object form (new, alongside existing string form):
```yaml
faults:
  db:
    - syscall: write
      action: delay
      delay: 500ms
      probability: 100%
    - syscall: connect
      action: deny
      errno: ECONNREFUSED
      probability: 10%
      trigger: { type: after, n: 2 }
```

String form (existing, still supported):
```yaml
faults:
  db:
    - "write=delay:500ms:100%"
    - "connect=ECONNREFUSED:10%:after=2"
```

Both parse to the same `FaultRule`. The YAML parser detects which form is used
(string vs map node).

### 6. Faults scoped per interface (future)

Not implemented in PoC, but the model supports it:
```yaml
faults:
  courier:
    internal:
      - { syscall: write, action: delay, delay: 2s, probability: 100% }
```

For PoC, faults apply to all interfaces of a service.

### 7. Compose-compatible naming

| Compose field | Faultbox field | Status |
|---------------|----------------|--------|
| `environment` | `environment`  | ✓ (alias for `env`) |
| `depends_on`  | `depends_on`   | ✓ (already used) |
| `healthcheck` | `healthcheck`  | ✓ (alias for `ready`) |
| `ports`       | via interfaces | Different model |
| `image`       | future         | Not in PoC |

## Protocol Layering (Architecture)

```
L4 Core (built-in):       tcp, udp
L7 Stdlib (built-in):     http, grpc
L7 Extensions (Starlark): postgres, kafka, redis, ...
```

For PoC: `tcp` and `http` are Go built-ins behind a `StepHandler` interface.
Same interface will be implemented by Starlark modules in future steps.

## Backward Compatibility

Old topology format (`port`, `env`, `ready`) is still accepted and auto-migrated:
- `port: N` → `interfaces: { default: { protocol: tcp, port: N } }`
- `env:` → `environment:`
- `ready:` → `healthcheck: { test: [...] }`

## PoC Scope

**In scope:**
- Multi-interface topology types + parsing
- Auto-injected env vars for service discovery
- Template resolution in env vars (`{{service.interface.addr}}`)
- `steps` in spec with http and tcp handlers
- Object-form fault parsing
- Backward compat with old format
- Config + step executor tests
- Updated example files

**Out of scope (future steps):**
- Starlark runtime / custom protocol modules
- Event log / trace-log
- Hooks (INIT/PRE/POST)
- Per-interface fault scoping
- gRPC, Kafka protocol support
- JSON Schema generation
- Container/image support

## Implementation Plan

1. Extend `internal/config/config.go` — new types, parsing, backward compat
2. Add `internal/config/config_test.go` — validation + parsing tests
3. Add `internal/config/resolve.go` — env var injection + template resolution
4. Add `internal/engine/step.go` — step executor with http/tcp handlers
5. Add `internal/engine/step_test.go` — step executor tests
6. Modify `internal/engine/simulation.go` — wire steps into runTrace()
7. Update `poc/example/` files
8. Test in Lima VM
