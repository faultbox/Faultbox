# Faultbox Spec Language Reference

Faultbox uses a single Starlark file (`faultbox.star`) to define the system
topology and test scenarios. Starlark is a Python-like language — the
configuration is code.

```bash
faultbox test faultbox.star                        # run all tests
faultbox test faultbox.star --test happy_path      # run one test
faultbox test faultbox.star --output trace.json    # JSON trace with syscall events
faultbox test faultbox.star --shiviz trace.shiviz  # ShiViz visualization format
faultbox test faultbox.star --runs 100 --show fail # counterexample discovery
faultbox test faultbox.star --seed 42              # deterministic replay
faultbox init --name orders --port 8080 ./order-svc  # generate starter .star
```

---

## Quick Start

```python
# faultbox.star

inventory = service("inventory", "/usr/local/bin/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders", "/usr/local/bin/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

def test_happy_path():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
    assert_true("confirmed" in resp.body)

    # Temporal: WAL must have been written.
    assert_eventually(service="inventory", syscall="openat", path="/tmp/inventory.wal")

def test_inventory_down():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # No WAL write should occur.
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)
```

---

## Topology

### `service(name, [binary], *interfaces, ...)`

Declares a service in the system under test. Returns a service object that can
be referenced by other services and used in tests.

A service must have exactly one source: `binary` (local executable), `image`
(Docker container image), or `build` (Dockerfile directory).

```python
# Binary mode — local executable
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    args = ["--data-dir", "/tmp/db-data"],
    env = {"PORT": "5432"},
    depends_on = [],
    healthcheck = tcp("localhost:5432"),
)

# Container mode — pull image from registry
postgres = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432"),
)

# Container mode — build from Dockerfile
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {"PORT": "8080", "DB_URL": postgres.main.internal_addr},
    depends_on = [postgres],
    healthcheck = http("localhost:8080/health"),
)
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Service name (used in logs and results) |
| `binary` | string | one of three | Path to the executable (positional or keyword) |
| `image` | string | one of three | Docker image reference (e.g., `"postgres:16-alpine"`) |
| `build` | string | one of three | Path to Dockerfile context directory |
| *positional* | interface | **yes** | One or more `interface()` declarations |
| `args` | list | no | Command-line arguments passed to the binary |
| `env` | dict | no | Environment variables |
| `volumes` | dict | no | Volume mounts `{host_path: container_path}` (container mode) |
| `depends_on` | list | no | Services that must start first |
| `healthcheck` | healthcheck | no | Readiness check (`tcp()` or `http()`) |
| `observe` | list | no | Event sources to attach (see [Event Sources](#event-sources)) |

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

Protocols are provided by **plugins** — Go implementations registered at
compile time. Each protocol defines its own step methods, healthcheck,
and response format. See [Protocols](#protocols) for the full list.

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

---

## Type Reference

Everything in a `.star` file is a typed value. This section defines each
built-in type, its constructor, properties, and what's extensible.

### Type: `Service`

**Constructor:** `service(name, [binary], *interfaces, ...)`

A service declaration. Created by `service()` and assigned to a variable.
The variable name is arbitrary — `"main"` is not special:

```python
db = service("db", ...)        # variable "db", service name "db"
my_pg = service("postgres", ...)  # variable "my_pg", service name "postgres"
```

**Properties (read-only):**

| Property | Type | Description |
|----------|------|-------------|
| `.name` | `string` | Service name (first arg to `service()`) |
| `.<interface_name>` | `InterfaceRef` | Reference to the named interface |

**Shorthand step methods:** When a service has exactly one interface,
its step methods are promoted to the service level:

```python
# These are equivalent when api has one interface:
api.public.get(path="/health")
api.get(path="/health")
```

**What's user-defined:** The service name and interface names are yours.
Nothing is built-in — `"main"`, `"public"`, `"internal"` are conventions,
not keywords.

---

### Type: `Interface`

**Constructor:** `interface(name, protocol, port, spec=)`

Declares a communication endpoint on a service. The `protocol` string
selects which plugin handles step methods and healthchecks.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Your name for this interface (arbitrary) |
| `protocol` | string | **yes** | Plugin name — determines available methods |
| `port` | int | **yes** | Port number |
| `spec` | string | no | Path to protocol spec file (OpenAPI, protobuf, Avro) |

**What's user-defined:** The name is yours. The protocol must match a
registered plugin (see [Protocols](#protocols)).

---

### Type: `InterfaceRef`

**Not constructed directly.** Returned when you access `service.interface_name`.

```python
db = service("db", ..., interface("main", "tcp", 5432))
ref = db.main  # ← this is an InterfaceRef
```

**Properties (read-only):**

| Property | Type | Description |
|----------|------|-------------|
| `.addr` | `string` | `"localhost:<port>"` — for healthchecks, test steps, binary-mode env |
| `.host` | `string` | `"localhost"` |
| `.port` | `int` | Port number |
| `.internal_addr` | `string` | Container-to-container address (`"servicename:<port>"` in Docker, same as `.addr` for binaries) |

**Step methods:** determined by the protocol plugin. Accessing a method name
returns a callable `StepMethod`:

```python
db.main.send(data="PING")       # tcp protocol → send()
api.public.post(path="/data")    # http protocol → post()
pg.main.query(sql="SELECT 1")   # postgres protocol → query()
```

**`.addr` vs `.internal_addr`:**

| | Binary mode | Container mode |
|---|---|---|
| `.addr` | `localhost:5432` | `localhost:<mapped_port>` |
| `.internal_addr` | `localhost:5432` | `db:5432` (Docker DNS) |

Use `.addr` for healthchecks and test steps (from the host).
Use `.internal_addr` in container `env` (service-to-service).

---

### Type: `StepMethod`

**Not constructed directly.** Returned when you access a method on an InterfaceRef.

```python
fn = api.public.post   # ← StepMethod
fn(path="/data")       # ← callable
```

All step methods return a `Response`. The available methods depend on
the protocol — see [Protocols](#protocols).

---

### Type: `Response`

**Returned by step methods.** Wraps the result of a protocol call.

| Property | Type | Description |
|----------|------|-------------|
| `.status` | `int` | Status code (HTTP status, or 0 for non-HTTP on success) |
| `.body` | `string` | Raw response body |
| `.data` | `dict/list` | **Auto-decoded** — JSON body parsed into native Starlark values |
| `.ok` | `bool` | `True` if the step succeeded |
| `.error` | `string` | Error message if `.ok` is `False` |
| `.duration_ms` | `int` | Step execution time in milliseconds |

**`.body` vs `.data`:** `.body` is always the raw string. `.data` is the
same content auto-decoded from JSON — you never need `json.decode()`:

```python
resp = pg.main.query(sql="SELECT * FROM users")
print(resp.body)           # '[{"id": 1, "name": "alice"}]'
print(resp.data[0]["name"])  # 'alice'
```

---

### Type: `StarlarkEvent`

**Passed to `where=` lambda predicates** in assertions and `events()`.

| Property | Type | Description |
|----------|------|-------------|
| `.seq` | `int` | Monotonic sequence number |
| `.service` | `string` | Service that produced the event |
| `.type` | `string` | Event type (`"syscall"`, `"stdout"`, `"wal"`, `"topic"`) |
| `.event_type` | `string` | PObserve dotted notation (`"syscall.write"`) |
| `.data` | `dict` | Auto-decoded payload (from JSON `"data"` field, or all fields) |
| `.fields` | `dict` | Raw string fields |
| `.first` | `StarlarkEvent/None` | In `assert_before` `then=` lambda: the matched first event |
| `.<field_name>` | `string` | Direct access to any field (e.g., `.decision`, `.label`) |

```python
assert_eventually(where=lambda e: e.type == "wal" and e.data["op"] == "INSERT")
assert_before(
    first=lambda e: e.data["op"] == "INSERT",
    then=lambda e: e.data["ref_id"] == e.first.data["id"],
)
```

---

## Protocols

Protocols are **Go plugins** registered at compile time via `init()`.
Each protocol defines which step methods are available on its interfaces.

The `protocol` string in `interface(name, protocol, port)` selects the plugin.
**You cannot define new protocols in Starlark** — they are Go code.
To add a protocol, implement the `protocol.Protocol` interface in Go.

### Built-in Protocols

| Protocol | Step Methods | Response `.data` | Healthcheck |
|----------|-------------|-----------------|-------------|
| `http` | `get`, `post`, `put`, `delete`, `patch` | raw body (auto-decoded if JSON) | HTTP GET 2xx-3xx |
| `tcp` | `send` | response line as string | TCP connect |
| `postgres` | `query`, `exec` | `[{col: val, ...}]` / `{rows_affected: N}` | TCP + Postgres ping |
| `redis` | `get`, `set`, `del`, `ping`, `keys`, `lpush`, `rpush`, `lrange`, `incr`, `command` | `{value: ...}` | TCP + PING/PONG |
| `mysql` | `query`, `exec` | `[{col: val, ...}]` / `{rows_affected: N}` | TCP + MySQL ping |
| `kafka` | `publish`, `consume` | `{published: true}` / `{topic, key, value, ...}` | TCP connect |
| `nats` | `publish`, `request`, `subscribe` | `{subject, data}` | TCP connect |
| `grpc` | `call` | `{method, raw}` | TCP connect |

### Protocol Step Method Reference

**http** — `interface("api", "http", 8080)`
```python
resp = svc.api.get(path="/users", headers={"Authorization": "Bearer ..."})
resp = svc.api.post(path="/users", body='{"name": "alice"}')
```

**tcp** — `interface("main", "tcp", 5432)`
```python
resp = svc.main.send(data="PING")  # returns string, not Response
```

**postgres** — `interface("main", "postgres", 5432)`
```python
resp = svc.main.query(sql="SELECT * FROM users WHERE id=1")
# resp.data = [{"id": 1, "name": "alice"}]
resp = svc.main.exec(sql="INSERT INTO users (name) VALUES ('bob')")
# resp.data = {"rows_affected": 1}
```

**redis** — `interface("main", "redis", 6379)`
```python
svc.main.set(key="user:1", value="alice")
resp = svc.main.get(key="user:1")
# resp.data = {"value": "alice"}
```

**kafka** — `interface("main", "kafka", 9092)`
```python
svc.main.publish(topic="events", data='{"type": "order"}', key="order-1")
resp = svc.main.consume(topic="events", group="test")
# resp.data = {"topic": "events", "key": "order-1", "value": "..."}
```

**nats** — `interface("main", "nats", 4222)`
```python
svc.main.publish(subject="orders.new", data='{"id": 1}')
resp = svc.main.request(subject="orders.get", data='{"id": 1}')
resp = svc.main.subscribe(subject="orders.*")
```

**grpc** — `interface("main", "grpc", 9090)`
```python
resp = svc.main.call(method="/package.Service/GetUser", body='{"id": 1}')
```

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
| `.internal_addr` | `"hostname:port"` | `db.main.internal_addr` → `"db:5432"` (container) or `"localhost:5432"` (binary) |

**Container networking:** For container services, `.internal_addr` returns
`<service-name>:<port>` — the Docker network hostname. Use this for
container-to-container references in env vars. `.addr` returns
`localhost:<mapped-port>` for test driver access.

#### Auto-Injected Variables

Faultbox injects `FAULTBOX_<SERVICE>_<INTERFACE>_*` env vars for every service:

```
FAULTBOX_DB_MAIN_ADDR=localhost:5432
FAULTBOX_DB_MAIN_HOST=localhost
FAULTBOX_DB_MAIN_PORT=5432
```

---

## Event Sources

Event sources capture non-syscall events (stdout, WAL changes, message
queues, log files) and emit them into the trace as first-class events.
They are attached to services via the `observe=` parameter.

```python
api = service("api", "./api",
    interface("public", "http", 8080),
    observe=[
        stdout(decoder=json_decoder()),
    ],
)

db = service("postgres",
    interface("main", "postgres", 5432),
    image="postgres:16",
    observe=[
        stdout(decoder=logfmt_decoder()),
        wal_stream(slot="faultbox"),
    ],
)
```

Events from sources have a type (`"stdout"`, `"wal"`, `"topic"`, `"tail"`,
`"poll"`) and are queryable by assertions and monitors — same as syscall events.

### Built-in Event Sources

| Source | Constructor | What it captures |
|--------|------------|-----------------|
| stdout | `stdout(decoder=)` | Service stdout lines, decoded per line |
| wal_stream | `wal_stream(slot=)` | Postgres logical replication (INSERT/UPDATE/DELETE) |
| topic | `topic(broker=, topic=, group=)` | Kafka/NATS topic messages |
| tail | `tail(path=)` | New lines appended to a file (inotify) |
| poll | `poll(url=, interval=)` | Periodic HTTP endpoint fetch |

### Decoders

Decoders parse raw bytes (a line of output, a message payload) into
structured event fields. The `"data"` field is auto-decoded on
`StarlarkEvent.data` — no `json.decode()` needed.

| Decoder | Constructor | Parses |
|---------|------------|--------|
| json | `json_decoder()` | JSON objects — top-level keys become fields |
| logfmt | `logfmt_decoder()` | `key=value key2="value 2"` pairs |
| regex | `regex_decoder(pattern=)` | Named capture groups from regex |

```python
# JSON: {"level":"INFO","msg":"started"} → e.data["level"] == "INFO"
observe=[stdout(decoder=json_decoder())]

# Logfmt: level=INFO msg="started" → e.data["msg"] == "started"
observe=[stdout(decoder=logfmt_decoder())]

# Regex: WAL: fsync /data/wal/001 → e.data["action"] == "fsync"
observe=[stdout(decoder=regex_decoder(pattern=r"WAL: (?P<action>\w+) (?P<path>.+)"))]
```

### Querying Event Source Events

Event source events work with all assertion and query functions:

```python
# Assert a WAL INSERT happened:
assert_eventually(where=lambda e: e.type == "wal" and e.data["op"] == "INSERT")

# Monitor stdout for errors:
monitor(lambda e: fail("unexpected error") if e.type == "stdout" and "ERROR" in e.data.get("level", ""))

# Query Kafka topic events:
msgs = events(where=lambda e: e.type == "topic" and e.data["topic"] == "orders.events")
```

---

## Scenarios & Generation

### `scenario(fn)`

Registers a function as a **happy-path scenario**. The function runs as a
test (like `test_*`) and is also available to `faultbox generate` for
automatic failure mutation.

```python
def order_flow():
    api.post(path="/orders", body='{"sku": "widget", "qty": 1}')
    api.post(path="/payments", body='{"order_id": 1, "amount": 100}')
    resp = api.get(path="/orders/1")
    assert_eq(resp.data["status"], "paid")

scenario(order_flow)  # runs as test_order_flow + registered for generator
```

Everything that works in `test_*` functions works inside scenarios:
`fault()`, `trace()`, `monitor()`, `assert_eventually()`, `parallel()`, etc.

### `faultbox generate`

Takes registered scenarios and systematically wraps each in every possible
fault — one mutation per scenario × dependency × failure mode:

```bash
faultbox generate faultbox.star
# → order_flow.faults.star
# → health_check.faults.star
```

Generated `.faults.star` files use `load()` to import topology and scenario
functions from the source file. See [CLI Reference](cli-reference.md) for
all flags.

### `load(filename, symbol1, symbol2, ...)`

Imports symbols from another `.star` file. The loaded file shares the same
runtime (service registry, builtins, event log).

```python
# failures.star
load("faultbox.star", "api", "db", "order_flow")

def test_gen_order_flow_db_down():
    fault(api, connect=deny("ECONNREFUSED", label="db down"), run=order_flow)
```

Paths are resolved relative to the loading file's directory. Modules are
cached — each file is executed at most once.

### `print(...)`

Outputs to stderr during test execution. Use for debugging event structures:

```python
resp = db.main.query(sql="SELECT * FROM users")
print(resp.data)       # shows the auto-decoded dict/list structure
print(resp.data[0])    # shows first row

writes = events(service="db", syscall="write")
print(len(writes), "writes recorded")
```

---

## Tests

Test functions are named `test_*` and discovered automatically. Each test
runs with fresh service instances (restarted between tests).
Scenario-registered functions also run as tests (as `test_<name>`).

```python
def test_happy_path():
    """Normal operation — all services healthy."""
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)
```

### Execution Order

For each test function:

```
1. Reset event log (fresh trace per test)
2. Wait for ports to be free (cleanup from previous test)
3. Start all services in dependency order
4. Wait for healthchecks to pass
5. Run the test function
6. Stop all services (SIGTERM → SIGKILL after 2s)
7. Capture syscall trace and report result
```

### Running Tests

```bash
faultbox test faultbox.star                        # all tests
faultbox test faultbox.star --test happy_path      # one test
faultbox test faultbox.star --debug                # verbose logging
faultbox test faultbox.star --output trace.json    # JSON trace output
faultbox test faultbox.star --shiviz trace.shiviz  # ShiViz output
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
resp = api.post(path="/orders", body='{"sku":"widget","qty":1}')
resp = api.post(path="/data", body="data", headers={"Authorization": "Bearer token"})
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | no | URL path (default: `"/"`) |
| `body` | string | no | Request body |
| `headers` | dict | no | HTTP headers |

**Response object:**

```python
resp = api.post(path="/orders", body='{"sku":"widget"}')
resp.status       # int — HTTP status code (200, 404, 500, ...)
resp.body         # string — response body (trimmed)
resp.ok           # bool — True if step succeeded
resp.error        # string — error message if step failed
resp.duration_ms  # int — request duration in milliseconds
```

### TCP Steps

Available on interfaces with `protocol: "tcp"`.

**Operation:** `send`

```python
resp = db.main.send(data="PING")    # returns response as string
assert_eq(resp, "PONG")

resp = db.main.send(data="CHECK widget")
assert_eq(resp, "100")
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
def test_inventory_slow():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"gadget","qty":1}')
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 400)
    fault(inventory, write=delay("500ms"), run=scenario)
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
delay("500ms", label="slow WAL")   # labeled for diagnostics
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `duration` | string | — | Go duration: `"500ms"`, `"2s"`, `"100us"` |
| `probability` | string | `"100%"` | Chance the fault fires |
| `label` | string | — | Human-readable label shown in trace output |

### `deny(errno, probability=, label=)`

Fails a syscall by returning an error code.

```python
deny("ECONNREFUSED")                     # 100% connection refused
deny("EIO", probability="10%")           # 10% I/O error
deny("ENOSPC")                           # disk full
deny("EIO", label="WAL write")           # labeled for diagnostics
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `errno` | string | — | Error code (see table below) |
| `probability` | string | `"100%"` | Chance the fault fires |
| `label` | string | — | Human-readable label shown in trace output |

**Labels in diagnostics:** When a labeled fault fires, the trace output shows
the label alongside the decision:
```
  syscall trace (85 events):
    #72  db    write   deny(input/output error)  [WAL write]
    #73  db    write   deny(input/output error)  [WAL write]
  fault rule on db: write=deny(EIO) → filter:[write,writev,pwrite64] label="WAL write"
```

### Fault Targeting

Keyword arguments map syscall names to faults:

```python
fault(inventory, write=delay("500ms"), run=fn)     # delay inventory's write() syscalls
fault(orders, connect=deny("ECONNREFUSED"), run=fn) # deny orders' connect()
fault(inventory, fsync=deny("EIO"), run=fn)         # fail inventory's fsync
fault(inventory, openat=deny("ENOSPC"), run=fn)     # fail inventory's file opens
```

Faults apply to the **service's own syscalls**:

```python
# CORRECT: orders can't connect to inventory (orders makes outbound connect)
fault(orders, connect=deny("ECONNREFUSED"), run=fn)

# CORRECT: inventory WAL write is slow (inventory's write syscall is delayed)
fault(inventory, write=delay("500ms"), run=fn)
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

## Protocol-Level Faults

Syscall-level `fault(service, ...)` operates at the kernel level. Protocol-level
`fault(interface_ref, ...)` operates at the application protocol level via a
transparent proxy.

### `fault(interface_ref, *rules, run=, source=)`

When the first argument is an **interface reference** (e.g., `db.main`),
Faultbox starts a transparent proxy that speaks the interface's protocol
and injects faults matching the rules.

```python
# Syscall level — first arg is service:
fault(db, write=deny("EIO"), run=scenario)

# Protocol level — first arg is interface_ref:
fault(db.main, error(query="INSERT*", message="disk full"), run=scenario)
fault(api.public, response(path="/orders", status=429), run=scenario)
fault(kafka.main, drop(topic="orders.*"), run=scenario)
```

Optional `source=` targets a specific consumer when multiple services
connect to the same interface:

```python
fault(kafka.main, source=worker,
    drop(topic="orders.*"),
    run=scenario,
)
```

### Protocol fault builtins

These create `ProxyFaultDef` values passed as positional args to `fault()`.
All support glob patterns for matching.

#### `response(method=, path=, status=, body=, command=, key=, value=)`

Return a custom response without forwarding to the real service.

```python
response(method="POST", path="/orders", status=429, body='{"error":"rate_limited"}')
response(command="GET", key="cache:*")               # Redis nil (empty body)
response(command="GET", key="cache:*", value="stale") # Redis custom value
```

#### `error(method=, path=, query=, command=, key=, topic=, message=, status=)`

Return a protocol-specific error.

```python
error(query="INSERT*", message="disk full")          # Postgres/MySQL
error(method="/pkg.Svc/Method", status=14)            # gRPC UNAVAILABLE
error(command="SET", key="session:*", message="READONLY")  # Redis
error(topic="orders.*", message="LEADER_NOT_AVAILABLE")    # Kafka
```

#### `delay(method=, path=, query=, command=, key=, topic=, delay=)`

Delay matching requests, then forward normally.

```python
delay(path="/data/*", delay="500ms")                 # HTTP
delay(query="SELECT*", delay="3s")                   # Postgres/MySQL
delay(command="GET", delay="2s")                     # Redis
delay(topic="orders.events", delay="5s")             # Kafka
```

> **Note:** `delay()` without a positional duration returns a protocol-level
> fault. `delay("500ms")` with a positional duration returns a syscall-level
> fault. Same builtin, context-dependent.

#### `drop(method=, path=, topic=, probability=)`

Drop the connection or message.

```python
drop(method="POST", path="/upload")                  # HTTP — TCP reset
drop(topic="orders.events", probability="30%")       # Kafka — message loss
```

#### `duplicate(topic=)`

Deliver a message twice (for idempotency testing).

```python
duplicate(topic="orders.events")                     # Kafka/NATS
```

### Supported protocols

| Protocol | Match by | Fault builtins |
|----------|----------|---------------|
| `http` | `method=`, `path=` | response, error, delay, drop |
| `postgres` | `query=` | error, delay, drop |
| `mysql` | `query=` | error, delay, drop |
| `redis` | `command=`, `key=` | error, response, delay, drop |
| `grpc` | `method=` | error, delay, drop |
| `kafka` | `topic=` | drop, delay, error, duplicate |
| `mongodb` | `method=` (cmd), `key=` (collection) | error, delay, drop |
| `amqp` | `topic=` (routing key) | drop, delay, error |
| `nats` | `topic=` (subject) | drop, delay |
| `memcached` | `command=`, `key=` | error, response, delay, drop |

### Trace events

Protocol proxy actions emit `type="proxy"` events into the trace:

```python
assert_eventually(where=lambda e: e.type == "proxy" and e.data.get("action") == "error")
```

---

## Assertions

Starlark has no built-in `assert` statement. Faultbox provides assertion
builtins — value checks, temporal properties, and ordering verification.

### Value Assertions

#### `assert_true(condition, message=)`

```python
assert_true(resp.status == 200)
assert_true("ok" in resp.body, "expected ok in body")
assert_true(resp.duration_ms < 1000, "response too slow")
```

#### `assert_eq(a, b, message=)`

```python
assert_eq(resp.status, 200)
assert_eq(resp.body, "hello")
assert_eq(db.main.send(data="PING"), "PONG")
```

### Temporal Assertions

Temporal assertions query the syscall event trace captured during the current
test. Every intercepted syscall is recorded with service attribution, decision,
and path — temporal assertions search this trace.

#### `assert_eventually(service=, syscall=, path=, decision=, where=)`

Asserts that **at least one** event matches all given filters.
Use this to verify that an expected operation occurred.

```python
# Simple filter matching:
assert_eventually(service="inventory", syscall="openat", path="/tmp/inventory.wal")
assert_eventually(service="inventory", syscall="fsync", decision="deny*")
assert_eventually(service="orders", syscall="connect")

# Lambda predicate for complex conditions:
assert_eventually(where=lambda e: e.service == "db" and e.data.get("table") == "users")
assert_eventually(where=lambda e: e.type == "wal" and e.data["op"] == "INSERT")
```

#### `assert_never(service=, syscall=, path=, decision=, where=)`

Asserts that **no** event matches all given filters.
Use this to verify that an operation did NOT occur.

```python
# Simple filter matching:
assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")
assert_never(service="db", syscall="write", decision="deny*")

# Lambda predicate:
assert_never(where=lambda e: e.decision.startswith("deny") and e.label == "critical path")
```

#### Filter parameters

Two ways to filter events — **dict matching** (simple) and **lambda
predicates** (powerful). Both can be combined.

**Dict matching** — keyword arguments as flat string filters:

| Parameter | Type | Description |
|-----------|------|-------------|
| `service` | string | Service name (e.g., `"inventory"`, `"orders"`) |
| `syscall` | string | Syscall name (e.g., `"write"`, `"openat"`, `"connect"`) |
| `path` | string | File path (for file syscalls like `openat`) |
| `decision` | string | Fault decision (e.g., `"allow"`, `"deny*"`, `"delay*"`) |

**Glob matching:** Values ending with `*` match as a prefix. Values starting
with `*` match as a suffix. Example: `decision="deny*"` matches
`"deny(ECONNREFUSED)"`, `"deny(EIO)"`, etc.

**Lambda predicate** — `where=lambda e: ...` for complex conditions:

The lambda receives a `StarlarkEvent` (see [Type Reference](#type-starlarkevent))
with `.service`, `.type`, `.data`, `.fields`, `.seq`, and direct field access.

```python
# Access auto-decoded structured data:
where=lambda e: e.data["table"] == "users" and e.data["op"] == "INSERT"

# Combine multiple conditions:
where=lambda e: e.service == "db" and int(e.fields.get("size", "0")) > 4096
```

### Ordering Assertions

#### `assert_before(first=, then=)`

Asserts that the first event matching `first` occurs before the first event
matching `then` in the trace. Arguments can be dicts (same filter keys as
`assert_eventually`) or lambda predicates.

```python
# Dict matching:
assert_before(
    first={"service": "inventory", "syscall": "openat", "path": "/tmp/inventory.wal"},
    then={"service": "inventory", "syscall": "write", "path": "/tmp/inventory.wal"},
)

# Lambda predicates with correlation — then= receives the matched first event:
assert_before(
    first=lambda e: e.data["op"] == "INSERT",
    then=lambda e: e.data["ref_id"] == e.first.data["id"],
)
```

### Event Query

#### `events(service=, syscall=, path=, decision=, where=)`

Returns a list of matching events from the current test's trace.
Each element is a `StarlarkEvent` with `.service`, `.type`, `.data`, `.fields`.

```python
# Dict matching:
retries = events(service="orders", syscall="connect", decision="deny*")
print("retries:", len(retries))

# Lambda predicate:
big_writes = events(where=lambda e: e.data.get("size", 0) > 4096)

```python
# Count how many connect retries happened.
retries = events(service="orders", syscall="connect", decision="deny*")
print("retries:", len(retries))

# Get all WAL operations.
wal_ops = events(service="inventory", path="/tmp/inventory.wal")
```

---

## Concurrency

### `parallel(fn1, fn2, ...)`

Runs multiple step callables concurrently. Returns results in argument order.
Use with `--runs N` to explore different interleavings — each seed produces
a different scheduling order.

```python
def test_concurrent_orders():
    """Two orders at once — no double-spend."""
    results = parallel(
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
    )
    ok_count = sum(1 for r in results if r.status == 200)
    assert_eq(ok_count, 1, "exactly one order should succeed")
```

```bash
faultbox test faultbox.star --runs 100 --show fail   # random interleavings
faultbox test faultbox.star --explore=all             # exhaustive: ALL permutations
faultbox test faultbox.star --explore=sample           # 100 random orderings
faultbox test faultbox.star --seed 42                  # replay exact ordering
```

### `nondet(service, ...)`

Excludes one or more services from interleaving control during `parallel()`.
Their syscalls proceed immediately without being held. Use this for services
that make nondeterministic background requests (healthchecks, metrics, logging).

```python
def test_concurrent_orders():
    nondet(monitoring_svc, cache_svc)  # exclude from ordering exploration
    results = parallel(
        lambda: orders.post(path="/orders", body='...'),
        lambda: orders.post(path="/orders", body='...'),
    )
```

---

## Virtual Time

When `--virtual-time` is enabled, fault delays advance a virtual clock instead
of sleeping on real wall-clock time. A test with `delay("2s")` completes in
milliseconds. This makes exhaustive exploration practical.

```bash
faultbox test faultbox.star --virtual-time                    # fast delays
faultbox test faultbox.star --virtual-time --explore=all      # fast + exhaustive
```

**Scope:** Virtual time applies to:
- Fault delays (`ActionDelay`) — skip sleep, advance clock
- `nanosleep`/`clock_nanosleep` syscalls — return immediately (for C/Rust targets)
- `clock_gettime` — return virtual timestamp (for C/Rust targets)

**Go targets limitation:** Go uses vDSO for `time.Now()` (no syscall, not interceptable)
and `futex` for `time.Sleep()`. Virtual time primarily speeds up fault delays,
which is the main bottleneck in multi-run exploration.

---

## Monitors

### `monitor(callback, service=, syscall=, path=, decision=)`

Registers a continuous monitor that is called on every matching syscall event
during the test. If the callback raises an error, the test fails with
"monitor violation".

```python
def check_no_unhandled_io_error(event):
    """Safety: no I/O errors should go unhandled."""
    if event["decision"].startswith("deny"):
        # This monitor fires on every denied syscall.
        # A real monitor would check application state here.
        pass

monitor(check_no_unhandled_io_error, service="inventory", decision="deny*")
```

Monitors are cleared between tests automatically.

---

## Network Partitions

### `partition(svc_a, svc_b, run=callback)`

Creates a bidirectional network partition between two services. While the
callback runs, `svc_a` cannot connect to `svc_b` and vice versa. Connections
are denied with `ECONNREFUSED` filtered by destination address.

```python
def test_network_partition():
    """Orders can't reach inventory — returns 503."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")
    partition(orders, inventory, run=scenario)
```

Unlike `fault(orders, connect=deny("ECONNREFUSED"))` which blocks **all**
outbound connections, `partition()` only blocks connections to the specific
service's ports — other connectivity remains unaffected.

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

## Trace Output

Every intercepted syscall is recorded in an ordered event log with:
- Sequential event number
- Timestamp
- Service name
- Syscall name, PID, decision, file path
- PObserve-compatible `event_type` and `partition_key`
- ShiViz-compatible vector clock

### JSON Trace (`--output trace.json`)

```json
{
  "version": 1,
  "star_file": "faultbox.star",
  "duration_ms": 1640,
  "pass": 2,
  "fail": 1,
  "tests": [
    {
      "name": "test_happy_path",
      "result": "pass",
      "seed": 0,
      "duration_ms": 225,
      "events": [
        {
          "seq": 1,
          "timestamp": "2026-03-25T19:15:07.547Z",
          "type": "service_started",
          "event_type": "lifecycle.started",
          "partition_key": "inventory",
          "service": "inventory",
          "vector_clock": {"inventory": 1}
        },
        {
          "seq": 42,
          "timestamp": "2026-03-25T19:15:07.650Z",
          "type": "syscall",
          "event_type": "syscall.openat",
          "partition_key": "inventory",
          "service": "inventory",
          "fields": {
            "syscall": "openat",
            "pid": "1234",
            "decision": "allow",
            "path": "/tmp/inventory.wal"
          },
          "vector_clock": {"inventory": 20, "orders": 5}
        }
      ]
    },
    {
      "name": "test_flaky",
      "result": "fail",
      "reason": "assert_true: expected 200 or 503, got 0",
      "failure_type": "assertion",
      "seed": 7,
      "duration_ms": 215,
      "replay_command": "faultbox test faultbox.star --test flaky --seed 7",
      "events": []
    }
  ]
}
```

**Agent loop fields:** Failed tests include `replay_command` (full CLI for
deterministic replay) and `failure_type` (`"assertion"`, `"timeout"`,
`"service_start"`, or `"error"`) for machine consumption.

### Event Types (PObserve-Compatible)

Events use dotted `event_type` for PObserve compatibility:

| Event Type | Description |
|------------|-------------|
| `lifecycle.started` | Service process launched |
| `lifecycle.ready` | Healthcheck passed |
| `syscall.write` | `write` syscall intercepted |
| `syscall.connect` | `connect` syscall intercepted |
| `syscall.openat` | `openat` syscall intercepted |
| `syscall.fsync` | `fsync` syscall intercepted |
| `step_send.<service>` | Test driver sent request to service |
| `step_recv.<service>` | Test driver received response from service |
| `fault_applied` | Fault rules activated on a service |
| `fault_removed` | Fault rules deactivated |

The `partition_key` field (default: service name) enables routing events to
per-service PObserve monitor instances.

### ShiViz Visualization (`--shiviz trace.shiviz`)

Produces a [ShiViz](https://bestchai.bitbucket.io/shiviz/)-compatible trace
file with vector clocks for visualizing causal relationships between services.

```
(?<host>\S+) (?<clock>\{.*\})

inventory {"inventory": 1}
lifecycle.started
orders {"orders": 1}
lifecycle.started
test {"test": 1}
step_send.orders post→orders
test {"test": 2, "inventory": 20, "orders": 15}
step_recv.orders post→orders
```

Vector clocks track causality:
- Each service increments its own clock on every syscall
- When service A makes a network call, remote clocks are merged
- When the test driver receives a step response, the target service's clock merges

Open the `.shiviz` file at https://bestchai.bitbucket.io/shiviz/ to see a
space-time diagram with communication arrows between services.

---

## CLI Summary

```bash
# Run tests
faultbox test faultbox.star                        # run all tests
faultbox test faultbox.star --test happy_path      # run one test
faultbox test faultbox.star --debug                # verbose logging
faultbox test faultbox.star --output trace.json    # JSON trace with events
faultbox test faultbox.star --shiviz trace.shiviz  # ShiViz visualization
faultbox test faultbox.star --normalize trace.norm # deterministic trace fingerprint

# Counterexample discovery (P-lang style)
faultbox test faultbox.star --runs 100 --show fail # run 100x, show failures only
faultbox test faultbox.star --seed 42              # replay with specific seed

# Exhaustive interleaving exploration
faultbox test faultbox.star --explore=all           # try all permutations (K!)
faultbox test faultbox.star --explore=sample         # 100 random orderings (default)
faultbox test faultbox.star --explore=sample --runs 500  # 500 random orderings

# Virtual time (skip fault delays)
faultbox test faultbox.star --virtual-time          # instant delay faults

# Compare traces
faultbox diff trace1.norm trace2.norm              # verify determinism

# Scaffolding
faultbox init --name orders --port 8080 ./order-svc  # generate starter .star
```

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | All tests passed |
| 1 | Faultbox error (bad config, load failure, etc.) |
| 2 | One or more tests failed |

### Trace Summary

After each test, Faultbox prints a compact trace summary showing only fault
events (non-allow decisions). Failed tests include seed for deterministic replay:

```
--- PASS: test_happy_path (225ms, seed=0) ---
  syscall trace (99 events):

--- PASS: test_inventory_slow (1724ms, seed=0) ---
  syscall trace (70 events):
    #57  inventory    write      delay(500ms)  (+500ms)
    #69  inventory    write      delay(500ms)  (+500ms)

--- FAIL: test_flaky_network (215ms, seed=7) ---
  reason: assert_true: expected 200 or 503, got 0
  replay: faultbox test faultbox.star --test flaky_network --seed 7
  syscall trace (46 events):
    #50  orders       connect    deny(connection refused)
```

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

Monitors (basic `monitor()` builtin) are already implemented — see the
Monitors section above. State machine hooks will extend monitors with
per-service state tracking and lifecycle-aware fault decisions.
