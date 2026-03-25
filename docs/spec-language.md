# Faultbox Spec Language Reference

Faultbox uses a single Starlark file (`faultbox.star`) to define the system
topology and test scenarios. Starlark is a Python-like language — the
configuration is code.

```bash
faultbox test faultbox.star                      # run all tests
faultbox test faultbox.star --test happy_path    # run one test
faultbox test faultbox.star --output results.json
```

---

## Quick Start

```python
# faultbox.star

db = service("db", "/usr/local/bin/my-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/usr/local/bin/my-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def test_happy_path():
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)

def test_db_down():
    def scenario():
        resp = api.post(path="/data/key1", body="value")
        assert_eq(resp.status, 500)
        assert_true("db error" in resp.body)
    fault(api, connect=deny("ECONNREFUSED"), run=scenario)
```

---

## Topology

### `service(name, binary, *interfaces, ...)`

Declares a service in the system under test. Returns a service object that can
be referenced by other services and used in tests.

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    depends_on = [],
    healthcheck = tcp("localhost:5432"),
)
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Service name (used in logs and results) |
| `binary` | string | **yes** | Path to the executable |
| *positional* | interface | **yes** | One or more `interface()` declarations |
| `env` | dict | no | Environment variables |
| `depends_on` | list | no | Services that must start first |
| `healthcheck` | healthcheck | no | Readiness check (`tcp()` or `http()`) |

Services must be declared in dependency order — define `db` before `api` if
`api` depends on `db`.

### `interface(name, protocol, port, spec=)`

Declares a communication interface for a service.

```python
interface("public", "http", 8080)
interface("main", "tcp", 5432)
interface("internal", "grpc", 9090)
interface("events", "kafka", 9092, spec="./events.avsc")
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Interface name (e.g., `"main"`, `"public"`) |
| `protocol` | string | **yes** | Protocol type (`"http"`, `"tcp"`, `"grpc"`, etc.) |
| `port` | int | **yes** | Port number |
| `spec` | string | no | Path to protocol spec file (OpenAPI, protobuf, Avro, etc.) |

Currently supported protocols for step execution: `http`, `tcp`.
Other protocols can be declared for documentation and future Starlark modules.

The `spec` field is a path to a file that a protocol module understands.
Faultbox core doesn't parse it — future Starlark protocol modules will.

### Multi-Interface Services

A service can expose multiple interfaces:

```python
courier = service("courier", "./courier-svc",
    interface("public", "http", 8080),
    interface("internal", "grpc", 9090),
    interface("events", "kafka", 9092),
    depends_on = [db, cache],
    healthcheck = http("localhost:8080/health"),
)
```

Access interfaces by name: `courier.public`, `courier.internal`, `courier.events`.

### Healthchecks

#### `tcp(addr, timeout=)`

```python
healthcheck = tcp("localhost:5432")
healthcheck = tcp("localhost:5432", timeout="15s")
```

Polls a TCP connection until it succeeds.

#### `http(url, timeout=)`

```python
healthcheck = http("localhost:8080/health")
healthcheck = http("localhost:8080/ready", timeout="30s")
```

Polls an HTTP endpoint until it returns 2xx/3xx.

Default timeout for both: `10s`.

### Environment Variables

#### User-Defined

```python
env = {"PORT": "8080", "LOG_LEVEL": "debug"}
```

#### Cross-Service References

Reference another service's interface address directly:

```python
api = service("api", "./api",
    interface("public", "http", 8080),
    env = {"DB_ADDR": db.main.addr},   # → "localhost:5432"
    depends_on = [db],
)
```

Available attributes on interface references:

| Attribute | Returns | Example |
|-----------|---------|---------|
| `.addr` | `"localhost:port"` | `db.main.addr` → `"localhost:5432"` |
| `.host` | `"localhost"` | `db.main.host` → `"localhost"` |
| `.port` | port number | `db.main.port` → `5432` |

#### Auto-Injected Variables

Faultbox injects `FAULTBOX_<SERVICE>_<INTERFACE>_*` env vars for every service:

```
FAULTBOX_DB_MAIN_ADDR=localhost:5432
FAULTBOX_DB_MAIN_HOST=localhost
FAULTBOX_DB_MAIN_PORT=5432
```

---

## Tests

Test functions are named `test_*` and discovered automatically. Each test
runs with fresh service instances (restarted between tests).

```python
def test_happy_path():
    """Normal operation — all services healthy."""
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)
```

### Execution Order

For each test function:

```
1. Wait for ports to be free (cleanup from previous test)
2. Start all services in dependency order
3. Wait for healthchecks to pass
4. Run the test function
5. Stop all services (SIGTERM → SIGKILL after 2s)
6. Report result
```

### Running Tests

```bash
faultbox test faultbox.star                      # all tests
faultbox test faultbox.star --test happy_path    # one test
faultbox test faultbox.star --debug              # verbose logging
faultbox test faultbox.star --output results.json
```

---

## Steps

Steps are method calls on service interfaces that exercise the running system.

### Step Addressing

```python
api.public.post(path="/data/key")   # explicit interface
api.post(path="/data/key")          # shorthand (single-interface service)
db.main.send(data="PING")           # TCP interface
```

When a service has one interface, the interface name can be omitted.

### HTTP Steps

Available on interfaces with `protocol: "http"`.

**Operations:** `get`, `post`, `put`, `delete`, `patch`

```python
resp = api.get(path="/health")
resp = api.post(path="/data/key1", body="hello world")
resp = api.post(path="/data/key1", body="data", headers={"Authorization": "Bearer token"})
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | no | URL path (default: `"/"`) |
| `body` | string | no | Request body |
| `headers` | dict | no | HTTP headers |

**Response object:**

```python
resp = api.post(path="/data/key1", body="hello")
resp.status       # int — HTTP status code (200, 404, 500, ...)
resp.body         # string — response body (trimmed)
resp.ok           # bool — True if step succeeded
resp.error        # string — error message if step failed
resp.duration_ms  # int — request duration in milliseconds
```

**Assertions use standard expressions:**

```python
assert_eq(resp.status, 200)
assert_true("stored" in resp.body)
assert_true(resp.duration_ms < 1000, "too slow")
assert_true(resp.ok)
```

No special `expect` fields — use Starlark expressions directly.

### TCP Steps

Available on interfaces with `protocol: "tcp"`.

**Operation:** `send`

```python
resp = db.main.send(data="PING")    # returns response as string
assert_eq(resp, "PONG")

resp = db.main.send(data="SET key value")
assert_eq(resp, "OK")
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `data` | string | **yes** | Data to send (newline appended automatically) |

TCP `send` returns a string (the first response line), not a response object.
It opens a connection, sends one line, reads one line, and closes.

---

## Faults

Faults inject failures at the syscall level via seccomp-notify.

### `fault(service, run=callback, **syscall_faults)`

Scoped fault injection — faults are active only during the callback:

```python
def test_db_slow():
    def scenario():
        resp = api.post(path="/data/key1", body="value")
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 400)
    fault(db, write=delay("500ms"), run=scenario)
```

The `run` parameter takes a callable. Faults are automatically removed when
the callback returns (even on error).

Multiple faults can be applied at once:

```python
fault(db,
    write=delay("1s"),
    connect=deny("ECONNREFUSED"),
    run=scenario,
)
```

### `fault_start(service, ...)` / `fault_stop(service)`

Imperative fault control:

```python
def test_imperative():
    fault_start(db, write=delay("500ms"))
    resp = api.post(path="/data/key1", body="value")
    assert_eq(resp.status, 200)
    fault_stop(db)
```

Use `fault()` with `run=` when possible — it guarantees cleanup.

### `delay(duration, probability=)`

Delays a syscall by sleeping before allowing it to proceed.

```python
delay("500ms")              # 500ms delay, 100% probability
delay("2s")                 # 2 second delay
delay("100ms", probability="50%")  # 50% chance of delay
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `duration` | string | — | Go duration: `"500ms"`, `"2s"`, `"100us"` |
| `probability` | string | `"100%"` | Chance the fault fires |

### `deny(errno, probability=)`

Fails a syscall by returning an error code.

```python
deny("ECONNREFUSED")           # 100% connection refused
deny("EIO", probability="10%") # 10% I/O error
deny("ENOSPC")                 # disk full
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `errno` | string | — | Error code (see table below) |
| `probability` | string | `"100%"` | Chance the fault fires |

### Fault Targeting

Keyword arguments map syscall names to faults:

```python
fault(db, write=delay("500ms"), run=fn)      # delay db's write() syscalls
fault(api, connect=deny("ECONNREFUSED"), run=fn)  # deny api's connect()
fault(db, openat=deny("ENOENT"), run=fn)     # fail db's file opens
fault(api, fsync=deny("EIO"), run=fn)        # fail api's fsync
```

Faults apply to the **service's own syscalls**:

```python
# CORRECT: API can't connect to DB (API makes outbound connect)
fault(api, connect=deny("ECONNREFUSED"), run=fn)

# CORRECT: DB responds slowly (DB's write to socket is delayed)
fault(db, write=delay("500ms"), run=fn)
```

### Supported Errno Values

**File/IO:** `ENOENT`, `EACCES`, `EPERM`, `EIO`, `ENOSPC`, `EROFS`, `EEXIST`,
`ENOTEMPTY`, `ENFILE`, `EMFILE`, `EFBIG`

**Network:** `ECONNREFUSED`, `ECONNRESET`, `ECONNABORTED`, `ETIMEDOUT`,
`ENETUNREACH`, `EHOSTUNREACH`, `EADDRINUSE`, `EADDRNOTAVAIL`

**Generic:** `EINTR`, `EAGAIN`, `ENOMEM`, `EBUSY`, `EINVAL`

### Supported Syscalls

**File/IO:** `openat`, `read`, `write`, `writev`, `readv`, `close`, `fsync`,
`mkdirat`, `unlinkat`, `faccessat`, `fstatat`, `getdents64`, `readlinkat`

**Network:** `connect`, `socket`, `bind`, `listen`, `accept`, `sendto`, `recvfrom`

**Process:** `clone`, `execve`, `wait4`, `getpid`, `getrandom`

---

## Assertions

Starlark has no built-in `assert` statement. Faultbox provides:

### `assert_true(condition, message=)`

```python
assert_true(resp.status == 200)
assert_true("ok" in resp.body, "expected ok in body")
assert_true(resp.duration_ms < 1000, "response too slow")
```

### `assert_eq(a, b, message=)`

```python
assert_eq(resp.status, 200)
assert_eq(resp.body, "hello")
assert_eq(db.main.send(data="PING"), "PONG")
```

### Using Starlark Expressions

Since the configuration is code, assertions are composable:

```python
# Compare strings
assert_true("error" in resp.body)
assert_true(resp.body.startswith("stored:"))

# Numeric comparisons
assert_true(resp.duration_ms > 400)
assert_true(resp.status >= 200 and resp.status < 300)

# Conditional logic
if resp.status != 200:
    print("unexpected status:", resp.status, "body:", resp.body)
    assert_true(False, "expected 200")
```

---

## Results (`results.json`)

When `--output` is specified, Faultbox writes structured JSON:

```json
{
  "simulation_id": "",
  "duration_ms": 1640,
  "traces": [
    {
      "name": "test_happy_path",
      "result": "pass",
      "duration_ms": 212
    },
    {
      "name": "test_db_slow",
      "result": "pass",
      "duration_ms": 1216
    },
    {
      "name": "test_api_cannot_reach_db",
      "result": "fail",
      "reason": "assert_eq failed: 200 != 500",
      "duration_ms": 212
    }
  ],
  "pass": 2,
  "fail": 1
}
```

---

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | All tests passed |
| 1 | Faultbox error (bad config, load failure, etc.) |
| 2 | One or more tests failed |

---

## Protocol Extensibility (Roadmap)

| Layer | Examples | Status |
|-------|----------|--------|
| **L4 Core** | `tcp` | Built-in |
| **L7 Stdlib** | `http` | Built-in |
| **L7 Extensions** | `grpc`, `postgres`, `kafka`, `redis` | Future: Starlark modules |

Protocol modules implement the same step interface. Usage won't change:

```python
# Future: redis.star loaded as module
cache.set(key="session:123", value="active")
cache.get(key="session:123")
```

---

## State Machines and Hooks (Roadmap)

Services will support state machines with lifecycle hooks:

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    states = ["starting", "ready", "degraded", "failed"],
    on_init = db_init,
    on_syscall = db_on_syscall,
)

def db_on_syscall(ctx, deps):
    if ctx.call.name == "write" and ctx.this.state == "degraded":
        return delay("2s")
    return allow()
```

Hooks receive a context with:
- `ctx.this` — current service (state, name, interfaces)
- `ctx.call` — syscall context (name, args, counter)
- `ctx.log` — global event log (emit + query)
- `deps` — dependency map

Monitors will check temporal properties over the event log:

```python
def monitor_liveness(ctx):
    for e in ctx.log.filter(type="request"):
        assert_true(ctx.log.exists(
            after=e, within="5s", type="response"
        ))
```
