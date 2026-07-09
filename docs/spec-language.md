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

## Primitive Index

Every builtin grouped by what it's for. Use Cmd-F to jump.

**Topology** —
[`service`](#service-topology),
[`mock_service`](#mock_service),
[`interface`](#interface),
[`tls_cert`](#tls-support) (RFC-038),
[`remotes`](#remotesdict) (RFC-036).

**Healthchecks** —
[`tcp`](#healthchecks),
[`http`](#healthchecks),
[`kafka_ready`](#healthchecks).

**Faults (syscall + protocol)** —
[`fault`](#fault),
[`fault_all`](#fault_all),
[`fault_start`](#fault_start--fault_stop),
[`fault_stop`](#fault_start--fault_stop),
[`fault_assumption`](#fault_assumption),
[`fault_scenario`](#fault_scenario),
[`fault_matrix`](#fault_matrix),
[`scenario`](#scenarios--generation),
[`partition`](#partition),
[`nondet`](#nondet).

**Fault primitives** —
[`deny`](#fault),
[`delay`](#fault),
[`allow`](#fault),
[`response`](#protocol-faults) (proxy),
[`error`](#protocol-faults) (proxy),
[`drop`](#protocol-faults) (proxy),
[`duplicate`](#protocol-faults) (proxy),
[`op`](#named-operations).

**Mock responses** —
[`json_response`](#response-constructors),
[`text_response`](#response-constructors),
[`bytes_response`](#response-constructors),
[`status_only`](#response-constructors),
[`redirect`](#response-constructors),
[`grpc_response`](#response-constructors),
[`grpc_typed_response`](#response-constructors) (RFC-023),
[`grpc_raw_response`](#response-constructors) (RFC-023),
[`grpc_error`](#response-constructors),
[`dynamic`](#response-constructors).

**Stdlib mocks** (under `@faultbox/mocks/`) —
[`kafka.broker`](#stdlib-mocks),
[`redis.server`](#stdlib-mocks),
[`mongo.server`](#stdlib-mocks),
[`grpc.server`](#stdlib-mocks) + 7 status shorthands (v0.9.8),
[`http.server`](#stdlib-mocks),
[`jwt.server`](#stdlib-mocks) (v0.9.9).

**Discovery helpers** (under `@faultbox/discovery/`, RFC-036) —
[`k8s.service`](#remote-services),
[`k8s.endpoint`](#remote-services),
[`k8s.local`](#remote-services).

**Spec-load file readers** (RFC-026, v0.9.8) —
[`load_file`](#file-readers--load_file-load_yaml-load_json-v098),
[`load_yaml`](#file-readers--load_file-load_yaml-load_json-v098),
[`load_json`](#file-readers--load_file-load_yaml-load_json-v098).

**Assertions** —
[`assert_true`](#assertions),
[`assert_eq`](#assertions),
[`assert_eventually`](#assertions),
[`assert_never`](#assertions),
[`assert_before`](#assertions).

**Matrix expectations** (RFC-027, v0.9.8) —
[`expect_success`](#expect_success--expect_error_withinms--expect_hang-v098),
[`expect_error_within`](#expect_success--expect_error_withinms--expect_hang-v098),
[`expect_hang`](#expect_success--expect_error_withinms--expect_hang-v098).

**Event sources & decoders** —
[`events`](#event-sources),
[`stdout`](#event-sources),
[`json_decoder`](#event-sources),
[`logfmt_decoder`](#event-sources),
[`regex_decoder`](#event-sources),
[`monitor`](#monitors).

**Concurrency primitives** —
[`parallel`](#concurrency).

**Tracing** —
[`trace`](#tracing),
[`trace_start`](#tracing),
[`trace_stop`](#tracing).

**Determinism** (RFC-040, v0.13.0) —
[`determinism`](#determinism-rfc-040),
[`service(nondeterministic_ok=...)`](#per-service-tolerance-nondeterministic_ok).

**Temporal primitives** (RFC-041, v0.13.0) —
[`eventually`](#eventuallypredicate-anchor),
[`always`](#alwayspredicate-between),
[`await_event`](#await_eventpredicate_or_matcher),
[`await_stable`](#await_stablequiescence_window-ignore),
[`test`](#testname-body-setup-expect-timeout-terminate_when-clock),
[`match`](#the-match-module).

**Misc** —
[`struct`](#struct---namespace-objects),
[`load`](#loadfilename-symbol1-symbol2-).

**Bottom-rung JWT primitives** (rarely needed; `jwt.server` is the
supported surface) —
[`jwt_keypair`](#stdlib-mocks),
[`jwt_sign`](#stdlib-mocks),
[`jwt_jwks`](#stdlib-mocks).

> The fastest way to look up a kwarg list: search for the function
> name in this file. Every primitive's section has a complete
> kwarg table or signature line. Anything missing is a doc bug —
> please file an issue.

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
| `healthcheck` | healthcheck | no | Readiness check (`tcp()`, `http()`, or `kafka_ready()`) |
| `observe` | list | no | Event sources to attach (see [Event Sources](#event-sources)) |
| `ports` | dict | no | Explicit host port mapping `{container_port: host_port}` (0 = Docker picks) |
| `reuse` | bool | no | Keep container alive between tests (see [Container Lifecycle](#container-lifecycle)) |
| `seed` | callable | no | Initialize service state after healthcheck — runs once (see [Container Lifecycle](#container-lifecycle)) |
| `reset` | callable | no | Re-initialize state between tests — runs before each test except the first (see [Container Lifecycle](#container-lifecycle)) |
| `ops` | dict | no | Named operations for fd-level fault targeting (see [Named Operations](#named-operations)) |
| `seccomp` | bool | no | Default `True`. Set to `False` to skip shim + seccomp-notify acquisition for this service. Proxy-level faults (HTTP/SQL/Redis/etc.) still apply; syscall-level `fault()` rules on this service are silently skipped. Workaround for multi-process container entrypoints (MySQL 8's `mysqld_safe` wrapper, certain JVM images) where the shim handoff hangs. Rejected on remote services. |
| `remote` | string \| `remotes()` | one of three* | Point at an externally-running endpoint instead of launching a process — typically a k8s Service hostname, a port-forwarded address, or an IP. Faultbox stands up its proxy in front of each interface; protocol-level faults work as usual; the SUT reaches the remote through the proxy. See [Remote Services](#remote-services) for the full rules. |

**Seed data for databases** — use `volumes` to mount init scripts:

```python
postgres = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    volumes = {"./init.sql": "/docker-entrypoint-initdb.d/init.sql"},
    healthcheck = tcp("localhost:5432"),
)
```

Most database images run scripts from `/docker-entrypoint-initdb.d/` on
first start. This creates your schema and test data before tests run.

Services must be declared in dependency order — define `db` before `api` if
`api` depends on `db`.

### `interface(name, protocol, port, spec=, tls=)`

Declares a communication interface for a service.

```python
interface("public", "http", 8080)
interface("main", "tcp", 5432)
interface("internal", "grpc", 9090)
interface("events", "kafka", 9092, spec="./events.avsc")

# TLS upstream — proxy terminates and re-establishes TLS at both legs:
interface("api", "https", 443, tls=tls_cert())
interface("geo", "grpc", 443, tls=tls_cert(ca="certs/upstream-ca.pem"))
```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Interface name (e.g., `"main"`, `"public"`) |
| `protocol` | string | **yes** | Protocol type (`"http"`, `"tcp"`, `"grpc"`, etc.) |
| `port` | int | **yes** | Port number |
| `spec` | string | no | Path to protocol spec file (OpenAPI, protobuf, Avro, etc.) |
| `tls` | `tls_cert(...)` | no | TLS material; the proxy terminates TLS at the listener and re-establishes it dialing upstream. See [TLS Support](#tls-support). |

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

### Remote Services

`service(remote=...)` (RFC-036, v0.12.29) declares a service whose process
lives outside Faultbox — typically a real pod in a k8s dev cluster you
can't pull as a Docker image. Faultbox does not launch the service. It
stands up its existing protocol proxy in front of each declared
interface and dials the remote upstream. The SUT reaches the remote
through the proxy, so every protocol-level fault — `response()`,
`error()`, `delay()`, gRPC method targeting, SQL matchers - fires
exactly as if it were a local container.

```python
load("@faultbox/discovery/k8s.star", "k8s")

geo = service("geo-config",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote      = k8s.service("geo-config", namespace = "staging"),
    healthcheck = http(k8s.endpoint("geo-config", 8080, namespace = "staging") + "/healthz"),
)
```

Behaviour:

- The `remote=` value is a plain hostname (e.g.
  `"geo.staging.svc.cluster.local"` or `"127.0.0.1"`) or a
  `host:port` string when you need to override the interface port.
  All interfaces share the host by default.
- For services whose interfaces live on different hosts, use
  `remotes({"iface": "host"})` — the keys must match declared interface
  names. Values are `host` (interface port appended) or `host:port`.
- `healthcheck=` is **required**. Faultbox can't infer "ready" for a
  pod it doesn't own; the spec must declare what readiness means.
- Cluster connectivity is your responsibility — `telepresence
  connect`, `kubectl port-forward`, in-cluster execution, or VPN. If
  the healthcheck fails on session start, the error includes a hint
  pointing at these workflows. See [docs/guides/connectivity.md] for
  the supported setups.
- Use `@faultbox/discovery/k8s.star` for the standard cluster-DNS
  string forms; nothing magic about it, just sugar over
  `<name>.<namespace>.svc.cluster.local`.

What's rejected on remote services (spec-load errors with explicit
messages naming the offending kwarg):

- Syscall-level faults (`write=deny()`, `connect=delay()`,
  `fsync=deny()`, etc.) — Faultbox can't seccomp a remote process.
  Move them to protocol faults (`response()` / `error()` / `delay()`)
  or use `mock_service()` if you need full process control.
- `seed=`, `reset=`, `reuse=` — Faultbox doesn't own the lifecycle.
- `volumes=`, `ports=`, `args=`, `binary=`, `image=`, `build=` —
  meaningless without a launched process.
- `seccomp=` — implied false; cannot be set.
- `observe=` — current event sources (`stdout`, `stderr`) require a
  process. (`topic()` and `poll()` will be allowed when they ship.)
- `ops=` — named-op grouping is a syscall-targeting feature.

Errors are deliberate and explicit ("I tried X, it silently did
nothing" is the failure mode this RFC is built to prevent).

#### `remotes(dict)`

Typed per-interface upstream-host map for `service(remote=...)`. Use
when interfaces of one logical service live on different hosts:

```python
geo = service("geo",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote = remotes({
        "public":   "geo.staging.svc.cluster.local",
        "internal": "geo-grpc.staging.svc.cluster.local:9999",
    }),
    healthcheck = http("geo.staging.svc.cluster.local:8080/healthz"),
)
```

Keys must match declared interface names. Values are `host` (interface
port appended at runtime) or `host:port` (used as-is).

#### Reproducibility caveat

A bundle from a run that used `remote=` services breaks the [`.fb`
bundle reproducibility contract](#bundles) — the remote pod is shared
and may drift between runs. The bundle records *that* remote services
were used (in `env.json` under `remotes: [...]`); `faultbox replay`
warns and re-dials them. Offline replay against recorded remote
responses is tracked in **RFC-037**.

For deterministic tests today, swap `remote=` for `mock_service()`
(same DSL surface elsewhere) and check the mock contract into git.

\* "one of three" / "one of four" — the source kwarg is exactly one of
`binary` / `image` / `build` / `remote`. Combining any two errors at
spec load.

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

**Constructor:** `interface(name, protocol, port, spec=, tls=)`

Declares a communication endpoint on a service. The `protocol` string
selects which plugin handles step methods and healthchecks.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | **yes** | Your name for this interface (arbitrary) |
| `protocol` | string | **yes** | Plugin name — determines available methods |
| `port` | int | **yes** | Port number |
| `spec` | string | no | Path to protocol spec file (OpenAPI, protobuf, Avro) |
| `tls` | `tls_cert(...)` | no | TLS material for the interface's proxy (RFC-038). See [TLS Support](#tls-support). |

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
| `.proxy_addr` | `string` | Host-side proxy listener for the SUT to dial (RFC-033). Late-bound — returns a placeholder at spec-load that the runtime resolves to e.g. `"127.0.0.1:36643"` once the proxy is up. |
| `.proxy_host` | `string` | Host part of `.proxy_addr` — `"127.0.0.1"` for binary SUTs, `"host.docker.internal"` for container SUTs. |
| `.proxy_port` | `string` | Port part of `.proxy_addr` (string, not int — see note below). |

**Step methods:** determined by the protocol plugin. Accessing a method name
returns a callable `StepMethod`:

```python
db.main.send(data="PING")       # tcp protocol → send()
api.public.post(path="/data")    # http protocol → post()
pg.main.query(sql="SELECT 1")   # postgres protocol → query()
```

**`.addr` vs `.internal_addr` vs `.proxy_addr`:**

| | Binary mode | Container mode |
|---|---|---|
| `.addr` | `localhost:5432` | `localhost:<mapped_port>` |
| `.internal_addr` | `localhost:5432` | `db:5432` (Docker DNS) |
| `.proxy_addr` | `127.0.0.1:<auto>` | `host.docker.internal:<auto>` |

Use `.addr` for healthchecks and test steps (from the host).
Use `.internal_addr` in container `env` for service-to-service traffic on the Docker network.
Use `.proxy_addr` / `.proxy_host` / `.proxy_port` to wire a SUT's connection through the
fault-injection proxy. **This is the right choice for any host-binary SUT connecting to a
Docker upstream** — the upstream's auto-mapped host port and the proxy's auto-assigned
listener port are both unknown at spec-load time, so a literal value would never work.

```python
truck = service("truck-api", "/usr/local/bin/truck-api",
    interface("main", "http", 9000),
    env = {
        "MYSQL_HOST": db.mysql.proxy_host,                      # → "127.0.0.1"
        "MYSQL_PORT": db.mysql.proxy_port,                      # → "36643" (string)
        "MYSQL_DSN":  "user:pass@tcp(" + db.mysql.proxy_addr + ")/appdb",
    },
)
```

**Late-binding mechanics:** at spec-load time `.proxy_addr` returns a placeholder string
(e.g. `__FB_PROXY_ADDR_db__mysql__`). The placeholder survives any string concatenation
the spec does. `buildEnv` replaces it with the real proxy address once the proxy starts.
**Don't `.split()` or `.rsplit()` the value** — operations on the placeholder run at
spec-load time and produce nonsense. Use `.proxy_host` / `.proxy_port` instead, which
are resolved separately.

**`.proxy_port` is a string, not an int.** Late-bound resolution can only substitute
into env strings, so the attribute returns a placeholder string at spec-load. Most
clients accept string ports; if your spec needs an int (rare), use the auto-injected
`FAULTBOX_<SVC>_<IFACE>_PORT` env var on the SUT process instead.

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
| `.<field_name>` | `string` | Direct access to any field (e.g., `.decision`, `.label`) |

```python
assert_eventually(where=lambda e: e.type == "wal" and e.data["op"] == "INSERT")

# assert_before takes dict filters (not lambdas):
assert_before(
    first={"service": "db", "syscall": "openat", "path": "/data/wal"},
    then={"service": "db", "syscall": "write", "path": "/data/wal"},
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
| `http2` | `get`, `post`, `put`, `delete`, `patch` | raw body | h2c GET 2xx-4xx |
| `tcp` | `send` | response line as string | TCP connect |
| `udp` | `send`, `send_no_reply` | `{raw: hex, size: N}` | UDP dial |
| `postgres` | `query`, `exec` | `[{col: val, ...}]` / `{rows_affected: N}` | TCP + Postgres ping |
| `redis` | `get`, `set`, `del`, `ping`, `keys`, `lpush`, `rpush`, `lrange`, `incr`, `command` | `{value: ...}` | TCP + PING/PONG |
| `mysql` | `query`, `exec` | `[{col: val, ...}]` / `{rows_affected: N}` | TCP + MySQL ping |
| `kafka` | `publish`, `consume` | `{published: true}` / `{topic, key, value, ...}` | TCP connect |
| `nats` | `publish`, `request`, `subscribe` | `{subject, data}` | TCP connect |
| `grpc` | `call` | `{method, raw}` | TCP connect |
| `mongodb` | `find`, `insert`, `insert_many`, `update`, `delete`, `count`, `command` | BSON docs normalized to JSON | TCP + Mongo ping |
| `cassandra` | `query`, `exec` | `[{col: val, ...}]` | TCP + CQL session |
| `clickhouse` | `query`, `exec` | `[{col: val, ...}]` / `{ok: true}` | HTTP /ping |

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

**http2** — `interface("public", "http2", 8080)`
```python
# Same API as HTTP/1.1; wire protocol is h2c (cleartext HTTP/2).
resp = svc.public.get(path="/users/1")
# resp.fields["proto"] = "HTTP/2.0"
```

**udp** — `interface("main", "udp", 8125)`
```python
svc.main.send_no_reply(data="api.requests:1|c")        # StatsD metric
resp = svc.main.send(hex="...", timeout_ms=2000)       # DNS query
# resp.data = {"raw": "<hex>", "size": 64}
```

**mongodb** — `interface("main", "mongodb", 27017)`
```python
db.main.insert(collection="users", document={"name": "alice", "role": "admin"})
resp = db.main.find(collection="users", filter={"role": "admin"}, limit=10)
# resp.data = [{"_id": "...", "name": "alice", "role": "admin"}]
db.main.command(cmd={"dropDatabase": 1})
```

**cassandra** — `interface("main", "cassandra", 9042)`
```python
cass.main.exec(cql="CREATE KEYSPACE IF NOT EXISTS test WITH replication = {...}")
resp = cass.main.query(cql="SELECT * FROM test.orders", consistency="QUORUM")
```

**clickhouse** — `interface("main", "clickhouse", 8123)` (HTTP interface)
```python
ch.main.exec(sql="INSERT INTO events (date, type) VALUES (today(), 'order')")
resp = ch.main.query(sql="SELECT count() as n FROM events")
# resp.data = [{"n": 1000000}]
```

### TLS Support

When `interface(..., tls=tls_cert(...))` is set, the Faultbox proxy
**terminates TLS at its listener** and **re-establishes TLS dialing the
upstream**. Between the two TLS legs the proxy sees plaintext, so all
the protocol-aware fault rules (`http.error(path=...)`,
`postgres.error(query=...)`, `redis.error(key=...)`, etc.) continue to
fire exactly the same as on plaintext upstreams.

This is the **opt-in TLS path** — without `tls=`, the proxy stays in
plain-TCP mode (the pre-RFC-038 behavior). Existing specs are
unchanged.

#### `tls_cert(...)` — TLS material for an interface

```python
tls_cert(
    proxy_cert = "certs/proxy-server.crt",   # cert proxy presents to clients
    proxy_key  = "certs/proxy-server.key",
    client_cert = "certs/proxy-client.crt",  # mTLS cert proxy uses upstream
    client_key  = "certs/proxy-client.key",
    ca = "certs/upstream-ca.pem",            # CA proxy trusts for upstream
    insecure = False,                        # InsecureSkipVerify on upstream
)
```

All kwargs are optional. `tls_cert()` (no args) is the dev/test
default — the proxy auto-generates a self-signed server cert in
memory, and trusts the system CA pool when verifying the upstream.

| Kwarg | Type | Default | Purpose |
|---|---|---|---|
| `proxy_cert` | string | auto-generated self-signed | Server cert the proxy presents to clients connecting to its listener. |
| `proxy_key` | string | (paired with `proxy_cert`) | Server key. Must be set if and only if `proxy_cert` is set. |
| `client_cert` | string | none | mTLS client cert the proxy presents when dialing the upstream. |
| `client_key` | string | (paired with `client_cert`) | mTLS client key. Must be set if and only if `client_cert` is set. |
| `ca` | string | system CA pool | PEM bundle the proxy trusts when verifying the upstream's cert. |
| `insecure` | bool | `False` | `InsecureSkipVerify` on the upstream side — dev escape hatch for self-signed upstreams. Mutually exclusive with `ca`. |

**Validation runs at spec-load time.** Half-set cert/key pairs,
missing files, garbage CA PEM, and `insecure=True` + `ca=`
collisions all fail with clear errors before the proxy starts.

**Relative paths resolve against the spec's directory.** Customers
usually keep cert material in a `certs/` subfolder next to the spec.

**`tls_cert()` is kwargs-only** — positional args are refused so a
typo can't silently swap server / client material.

#### Per-plugin TLS support matrix

The proxy has 14 plugins. As of v0.12.28, six terminate TLS; the
rest stay plain-TCP and emit a `proxy_tls_pending` warning when an
interface declares `tls=`. The deferred plugins are tracked in
[RFC-039](https://github.com/faultbox/Faultbox/issues/106).

| Protocol | TLS support | Pattern | Notes |
|---|---|---|---|
| `http` | ✅ v0.12.24 | wrap-and-dial | HTTPS; `Transport.TLSClientConfig` upstream. |
| `http2` | ✅ v0.12.24 | wrap-and-dial | ALPN `h2` forced; `http2.ConfigureServer` for dispatch. |
| `grpc` | ✅ v0.12.25 | framework creds | `grpc.Creds(credentials.NewTLS(...))` rather than listener-wrap (gRPC owns its handshake). |
| `kafka` | ✅ v0.12.26 | wrap-and-dial | Brokers expose plain + TLS on separate ports. |
| `redis` | ✅ v0.12.27 | wrap-and-dial | Redis 6+ `tls-port`. RESP3 corpus unchanged. |
| `tcp` | ✅ v0.12.28 | wrap-and-dial | Generic escape hatch for any "TLS from byte 1" service. Prefix-peek rules still fire on plaintext. |
| `postgres` | 🟡 deferred | upgrade-in-band | SSLRequest preamble. RFC-039 PR 1. |
| `mysql` | 🟡 deferred | upgrade-in-band | `CLIENT_SSL` capability. RFC-039 PR 2. |
| `mongodb` | 🟡 deferred | wrap-and-dial | RFC-039 PR 3. |
| `cassandra` | 🟡 deferred | wrap-and-dial | RFC-039 PR 4. |
| `clickhouse` | 🟡 deferred | wrap-and-dial | RFC-039 PR 4. |
| `nats` | 🟡 deferred | wrap-and-dial | RFC-039 PR 5. |
| `amqp` | 🟡 deferred | wrap-and-dial | RFC-039 PR 5. |
| `memcached` | 🟡 deferred | wrap-and-dial | RFC-039 PR 5. |
| `udp` | ❌ no TLS | — | UDP has no TLS in the kernel sense. DTLS would be a separate RFC. |

When an interface declares `tls=` against a 🟡 deferred plugin, the
proxy still starts but the listener stays plaintext. The runtime
emits a `proxy_tls_pending` event (visible in the bundle) and a
warning to stderr so the discrepancy is visible — silence here would
let "TLS handshake fails against proxy" debugging burn an hour.

#### Common patterns

**Dev / test against a TLS upstream with no cert management:**
```python
api = service("api", remote="api-prod.example.com",
    interface("public", "http", 443, tls=tls_cert()),
)
```
The proxy auto-generates its server cert; the upstream is verified
against the system CA pool.

**mTLS upstream (the inDrive Freight pattern):**
```python
geo = service("geo", remote="geo-config.svc.cluster.local",
    interface("api", "grpc", 443,
        tls = tls_cert(
            client_cert = "certs/proxy-client.crt",
            client_key  = "certs/proxy-client.key",
            ca = "certs/upstream-ca.pem",
        ),
    ),
)
```
The proxy presents its auto-cert to the SUT and `client_cert` to
the upstream.

**TLS-terminated upstream that uses a self-signed cert:**
```python
cache = service("cache", remote="redis-staging.local",
    interface("main", "redis", 6380, tls=tls_cert(insecure=True)),
)
```
`insecure=True` is logged at spec-load — use only for dev.

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

#### `kafka_ready(addr, timeout=)`

```python
healthcheck = kafka_ready("localhost:9092")
healthcheck = kafka_ready("localhost:9092", timeout="120s")
```

Verifies Kafka broker readiness at the protocol level: connects, creates a
sentinel topic, and confirms a partition leader is elected. More reliable than
`tcp()` for Kafka because Docker's port proxy accepts TCP before the broker is
ready to handle produce/consume requests. Default timeout: `120s`.

Default timeout for `tcp()` and `http()`: `10s`.

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

Since v0.9.5 (RFC-024), these values **point at a pass-through proxy**
that Faultbox pre-starts for every proxy-capable interface, not at the
real upstream. The proxy is transparent when no rules are installed —
behaviour is byte-identical to dialing the upstream directly. When
`fault(interface_ref, response(...)|error(...)|drop(...))` installs a
rule, the proxy applies it to the SUT's app-initiated traffic, not just
traffic from test-body requests. User env values that contain a literal
upstream address (e.g. `DATABASE_URL="postgres://u:p@localhost:5432/db"`
via `pg.main.addr` concatenation) are substring-rewritten the same way.

---

## Container Lifecycle

### Reuse, Seed, and Reset

By default, containers are created and destroyed for each test. For real
infrastructure (Postgres, Redis, Kafka), this can take 20+ seconds per test.

With `reuse=True`, containers are created once, seeded once, and reset
between tests — cutting multi-test execution time by 5-10x:

```python
postgres = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16-alpine",
    reuse = True,
    seed = seed_db,
    reset = reset_db,
    healthcheck = tcp("localhost:5432"),
)

def seed_db():
    """Runs once after first healthcheck — expensive initialization."""
    postgres.main.exec(sql="CREATE TABLE orders (id SERIAL, status TEXT)")
    postgres.main.exec(sql="INSERT INTO orders (status) VALUES ('pending')")

def reset_db():
    """Runs before each test (except first) — lightweight cleanup."""
    postgres.main.exec(sql="TRUNCATE orders RESTART IDENTITY CASCADE")
```

### Lifecycle

```
Suite start:  create → healthcheck → seed()
Test 1:       run test
              ↓ faults cleared, reset()
Test 2:       run test
              ↓ faults cleared, reset()
Test N:       run test
Suite end:    destroy container
```

- **seed()** runs once after the first healthcheck. Use it for schema creation,
  fixture data, or other expensive initialization.
- **reset()** runs before each subsequent test, after fault rules are cleared.
  Use it for `TRUNCATE`, `FLUSHDB`, or other fast state cleanup.
- If **reset** is not set, **seed** is called as a fallback between tests.
- If neither is set, a warning is emitted (state may leak between tests).
- Reset failure fails the entire test (prevents hidden state leak bugs).

### Ports

Use `ports=` to map container ports to specific host ports:

```python
kafka = service("kafka",
    interface("main", "kafka", 9092),
    image = "apache/kafka:3.7.0",
    ports = {9092: 9092},          # container:host
    healthcheck = kafka_ready("localhost:9092"),
)
```

When `ports` is not set, Docker picks random host ports automatically.

---

## Mock Services

For dependencies that don't deserve a full container (auth/JWKS stubs,
metrics sinks, feature-flag gateways) Faultbox can stand up
in-process protocol stubs entirely from Starlark — no Dockerfile,
no sidecar process. See the dedicated **[Mock Services reference](mock-services.md)**
for the full API. Quick summary:

```python
# Generic primitive — request/response protocols (HTTP, HTTP/2, TCP, UDP, gRPC).
auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration/jwks": json_response(200, {"keys": [...]}),
        "POST /token": dynamic(lambda req: json_response(200, {"sub": req["query"]["user"]})),
    },
)

# Stdlib mocks for stateful protocols.
load("@faultbox/mocks/kafka.star",   "kafka")
load("@faultbox/mocks/redis.star",   "redis")
load("@faultbox/mocks/mongodb.star", "mongo")

bus   = kafka.broker("bus",       interface = interface("main", "kafka", 9092),  topics = {"orders": []})
cache = redis.server("cache",     interface = interface("main", "redis", 6379),  state  = {"flag:new": "true"})
users = mongo.server("users-stub", interface = interface("main", "mongodb", 27017),
                                  collections = {"users": [{"_id": "1", "name": "alice"}]})
```

### `mock_service(name, *interfaces, routes={}, default=None, tls=False, config={}, descriptors=None, openapi=None, examples="first", validate="off", overrides={}, depends_on=[])`

Generic primitive for request/response protocols. Returns a `ServiceDef`
interchangeable with real services — `fault()`, `events()`, env-var
references all work the same way.

| Param | Notes |
|---|---|
| `routes` | Pattern → response dict. Pattern format depends on protocol (`"METHOD /path"` for HTTP, `"/pkg.Svc/Method"` for gRPC, byte-prefix string for TCP/UDP). OpenAPI-style `{id}` segments in HTTP patterns normalise to `*`. |
| `default` | Fallback when no route matches (default: protocol-appropriate error like HTTP 404). |
| `tls` | When True, terminate TLS using a per-runtime mock CA. CA bundle path available via the runtime; SUTs trust it via `RootCAs`. |
| `config` | Opaque dict consumed by the protocol plugin. Used by stdlib wrappers — you rarely set this directly. |
| `descriptors` | (gRPC only, RFC-023) Path to a `FileDescriptorSet` (protoc `--descriptor_set_out`). Enables typed-proto responses. |
| `openapi` | (HTTP only, RFC-021) Path to an OpenAPI 3.0 document. Faultbox generates one route per path × method, using the declared `example` as the response body. Loaded and validated at spec-load time. |
| `examples` | (HTTP only) Response-selection strategy: `"first"` (default, deterministic), `"<name>"` (pick named variant across ops), `"random"` (seeded random per op), `"synthesize"` (minimal type-correct values when no example is declared). |
| `validate` | (HTTP only) Request validation: `"off"` (default), `"warn"` (log mismatches), `"strict"` (reject with HTTP 400). Only JSON bodies are validated. |
| `overrides` | (HTTP only) Route dict that REPLACES OpenAPI-generated entries by pattern. Accepts OpenAPI-style paths (`{id}`). |
| `depends_on` | Same start-ordering semantics as `service()`. |

### Response constructors

```python
json_response(status=200, body={...}, headers={})    # JSON body, sets Content-Type
text_response(status=200, body="...", headers={})    # text/plain
bytes_response(status=0, data="raw bytes")           # TCP/UDP write-back
status_only(code)                                     # HTTP status, empty body
redirect(location, status=302)                        # HTTP redirect
grpc_response(body={...})                             # google.protobuf.Struct
grpc_error(code="UNAVAILABLE", message="...")         # gRPC canonical status
dynamic(fn)                                           # per-request callable (req → response)
```

`dynamic(fn)` runs a Starlark callable per request. The callable
receives a dict with `method`, `path`, `headers`, `query`, `body`
and returns a response value. Use it for JWT signing, request-aware
flag lookups, anything where the canned answer depends on the input.

### Stdlib mocks (`@faultbox/mocks/*.star`)

| Module | Constructor | Backed by |
|---|---|---|
| `@faultbox/mocks/kafka.star` | `kafka.broker(name, interface, topics, partitions, depends_on)` | `franz-go/pkg/kfake` — full broker |
| `@faultbox/mocks/redis.star` | `redis.server(name, interface, state, depends_on)` | `miniredis` — full RESP2 |
| `@faultbox/mocks/mongodb.star` | `mongo.server(name, interface, collections, depends_on)` | hand-written BSON OP_MSG/OP_QUERY responder |
| `@faultbox/mocks/grpc.star` | `grpc.server(name, interface, descriptors, services, depends_on, tls)` | protoreflect + FileDescriptorSet (RFC-023) |
| `@faultbox/mocks/http.star` | `http.server(name, interface, openapi, examples, validate, overrides, routes, default, depends_on, tls)` | kin-openapi + OpenAPI 3.0 (RFC-021) |
| `@faultbox/mocks/jwt.star` | `jwt.server(name, interface, issuer, key_id, depends_on)` → struct with `.service` / `.sign(claims=…)` / `.jwks` | Auto-generated EdDSA keypair + standard JWKS endpoint (v0.9.9, customer ask B3) |

**gRPC status-code shorthands (v0.9.8):** the `grpc` stdlib now exposes
per-code helpers so you don't have to remember the `grpc_error(code="…")`
incantation. Each wraps `grpc_error()` with a sensible default message.

```python
load("@faultbox/mocks/grpc.star", "grpc")

grpc.server(
    name        = "users",
    interface   = interface("main", "grpc", 50051),
    descriptors = "./proto/users.pb",
    services    = {
        "/users.v1.Users/Get":       {"response": {"id": 1, "name": "Alice"}},
        "/users.v1.Users/Admin":     grpc.permission_denied("admin only"),
        "/users.v1.Users/Slow":      grpc.deadline_exceeded(),
        "/users.v1.Users/Outage":    grpc.unavailable(),
    },
)
```

Available: `grpc.unavailable()`, `grpc.deadline_exceeded()`,
`grpc.permission_denied()`, `grpc.unauthenticated()`, `grpc.not_found()`,
`grpc.resource_exhausted()`, `grpc.internal()`.

Stdlib constructors are thin Starlark wrappers around `mock_service()`
that translate protocol-specific kwargs into the opaque `config=` map.
Same Go runtime, same event log, same `fault()` integration — just a
nicer call site for protocols where `routes={}` doesn't fit.

See the [Mock Services reference](mock-services.md) for the per-protocol
matrix, scope, and what mocks deliberately don't do.

---

## Event Sources

Event sources capture non-syscall events (stdout, WAL changes, message
queues, log files) and emit them into the trace as first-class events.
They are attached to services via the `observe=` parameter.

```python
api = service("api", "./api",
    interface("public", "http", 8080),
    observe=[
        observe.stdout(decoder=decoder("json")),
    ],
)

db = service("postgres",
    interface("main", "postgres", 5432),
    image="postgres:16",
    observe=[
        observe.stderr(decoder=decoder("logfmt")),
    ],
)
```

Events from sources have a type (`"stdout"`, `"stderr"`, and - once the
Go-plugin sources below ship a Starlark constructor - `"wal"`, `"topic"`,
`"tail"`, `"poll"`) and are queryable by assertions and monitors - same
as syscall events.

### Built-in Event Sources

Only `observe.stdout` and `observe.stderr` have Starlark constructors
today. The remaining sources exist as Go event-source plugins but have
no `observe.*` builtin yet - the factories are reserved (RFC-044 §8.6)
and will plug into the `observe` namespace in a later release.

| Source | Constructor | What it captures |
|--------|------------|-----------------|
| stdout | `observe.stdout(decoder=)` | Service stdout lines, decoded per line |
| stderr | `observe.stderr(decoder=)` | Service stderr lines, decoded per line (zap/logrus default) |
| wal_stream | Go plugin - no Starlark constructor yet (planned) | Postgres logical replication (INSERT/UPDATE/DELETE) |
| topic | Go plugin - no Starlark constructor yet (planned) | Kafka/NATS topic messages |
| tail | Go plugin - no Starlark constructor yet (planned) | New lines appended to a file (inotify) |
| poll | Go plugin - no Starlark constructor yet (planned) | Periodic HTTP endpoint fetch |

### Decoders

Decoders parse raw bytes (a line of output, a message payload) into
structured event fields. The `"data"` field is auto-decoded on
`StarlarkEvent.data` — no `json.decode()` needed.

| Decoder | Constructor (RFC-044) | Legacy alias (deprecated, removed in v0.14.0) | Parses |
|---------|----------------------|-----------------------------------------------|--------|
| json | `decoder("json")` | `json_decoder()` | JSON objects — top-level keys become fields |
| logfmt | `decoder("logfmt")` | `logfmt_decoder()` | `key=value key2="value 2"` pairs |
| regex | `decoder("regex", pattern=...)` | `regex_decoder(pattern=...)` | Named capture groups from regex |

```python
# JSON: {"level":"INFO","msg":"started"} → e.data["level"] == "INFO"
observe=[observe.stdout(decoder=decoder("json"))]

# Logfmt: level=INFO msg="started" → e.data["msg"] == "started"
observe=[observe.stdout(decoder=decoder("logfmt"))]

# Regex: WAL: fsync /data/wal/001 → e.data["action"] == "fsync"
observe=[observe.stdout(decoder=decoder("regex", pattern=r"WAL: (?P<action>\w+) (?P<path>.+)"))]
```

> **RFC-044 §8.6 + §8.7 migration:** v0.13.0 introduces the `observe` namespace (`observe.stdout`, `observe.stderr`) and the unified `decoder("name", ...)` dispatcher. The pre-rc2 top-level `stdout()` / `stderr()` and the three `*_decoder()` builtins remain registered as deprecated aliases that emit a one-time stderr warning **per process** (not per spec — a test harness that loads multiple specs sequentially sees the warning once total); they will be removed in **v0.14.0**. New specs should use the namespaced form; existing specs migrate by mechanical substitution.

### Querying Event Source Events

Event source events work with all assertion and query functions:

```python
# Assert a WAL INSERT happened (wal events require the wal_stream Go plugin):
assert_eventually(where=lambda e: e.type == "wal" and e.data["op"] == "INSERT")

# Monitor stdout for errors - RFC-041 monitor form (check=False FAILs):
monitor("no_stdout_errors",
    on    = match.event(type="stdout"),
    check = lambda event, state: "ERROR" not in event.data.get("level", ""),
)

# Query Kafka topic events (topic events require the topic Go plugin):
msgs = events(where=lambda e: e.type == "topic" and e.data["topic"] == "orders.events")
```

### Diagnosing SUT failures via `stdout` / `stderr`

When a containerized or binary SUT silently hangs at startup or fails
behind a proxy, attach `observe=[observe.stdout(decoder=...)]` (or
`observe.stderr(...)` if your service writes logs to fd 2) so the SUT's
own log lines become **first-class trace events** in the bundle. The bundle
becomes self-diagnosing — you can see the last function the SUT
reached without redeploying a debug build.

```python
# zap, logrus, slog (default) all write to stderr - capture via
# observe.stderr.
api = service("truck-api",
    interface("http", "http", 8080),
    binary="/usr/local/bin/truck-api",
    env={
        "DATABASE_HOST": db.mysql.proxy_host,
        "DATABASE_PORT": db.mysql.proxy_port,
    },
    observe=[observe.stderr(decoder=decoder("json"))],
    healthcheck=http("localhost:8080/health", timeout="60s"),
)

# Services that route logs to stdout explicitly (Python defaults,
# many CLIs) use observe.stdout - same surface, different fd.
worker = service("worker",
    interface("rpc", "tcp", 9000),
    binary="/usr/local/bin/worker",
    observe=[observe.stdout(decoder=decoder("logfmt"))],
)

# Capture both — the SUT writes errors to stderr, business events to
# stdout. Each emits with its own event type so you can filter the
# timeline and event log independently.
mixed = service("mixed",
    interface("api", "http", 8080),
    binary="/usr/local/bin/mixed",
    observe=[
        observe.stdout(decoder=decoder("json")),
        observe.stderr(decoder=decoder("json")),
    ],
)
```

Pre-v0.12.17 only `stdout()` existed; capturing zap/logrus output
required a SUT-side env-gate (e.g. `FB_LOG_TO_STDOUT=1`) to redirect
logs to fd 1. With `stderr()` you can capture default-configured Go
services without touching their code.

Pre-v0.12.19 both sources only worked against binary-mode services;
from v0.12.19 they apply to container services too. Faultbox reads
Docker's multiplexed log stream (`client.ContainerLogs(...)`) and
demultiplexes internally, so a containerised SUT becomes self-
diagnosing without any image change.

Combined with structured logging in the SUT (zap, slog, logrus, etc.),
this turns "the SUT hangs and nobody knows why" into "seq 33: 'done
init config' → seq 34: 'FATAL: connect to db: invalid connection'."
The latter is actionable from the bundle alone — no SUT
re-instrumentation needed across debug iterations.

This pattern was load-bearing for the v0.12.15.x diagnostic arc — three
proxy bugs (handshake, RESP3 framing, goroutine ctx-rooting) each
diagnosed from a customer bundle on the first attempt because the SUT's
fatal log was already in the trace. If you author specs that go to
customers, default to including `observe=[observe.stdout(decoder=...)]`
on the SUT - it's cheap to keep on, expensive to add later when
something breaks.

**Decoder choice:**

- `decoder("json")` - structured loggers (zap, zerolog, slog default)
- `decoder("logfmt")` - `key=value` style (logrus, klog)
- `decoder("regex", pattern=...)` - unstructured logs; capture the fields you need

If your SUT defaults to a non-stdout sink (file, syslog, proprietary
format), gate the stdout output behind an env var so production behavior
is unaffected — the spec sets the env var, the SUT honors it only under
test.

---

## Scenarios & Generation

### `scenario(fn)`

Registers a function as a **scenario probe**. The function runs as a test
(like `test_*`) and is also available to `faultbox generate`, `fault_scenario()`,
and `fault_matrix()`.

A scenario is a **probe** — it exercises the system and returns an observable
result. Scenarios SHOULD return values (for use with `fault_scenario(expect=)`)
and SHOULD NOT contain `assert_*` calls. Assertions belong in the `expect`
callback of `fault_scenario()` or `fault_matrix()`.

```python
def order_flow():
    """Place an order — returns response for external validation."""
    return orders.post(path="/orders", body='{"sku":"widget","qty":1}')

scenario(order_flow)  # runs as test_order_flow + registered for composition
```

Multi-step scenarios return a dict of observables:

```python
def order_lifecycle():
    place = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    if place.status != 200:
        return {"phase": "place", "resp": place}
    check = orders.get(path="/inventory/widget")
    return {"phase": "check_stock", "resp": check, "order": place}

scenario(order_lifecycle)
```

**Backward compatibility:** Existing scenarios with inline `assert_*` calls
still work — the return value is simply `None`. The convention of returning
values is optional but recommended for composition with `fault_scenario()`.

### `faultbox generate`

Takes registered scenarios and systematically generates `fault_assumption()`
definitions and a `fault_matrix()` call — one assumption per dependency ×
failure mode:

```bash
faultbox generate faultbox.star
# → order_flow.faults.star
# → health_check.faults.star
```

Generated `.faults.star` files use `load()` to import topology and scenario
functions, then define fault assumptions and a matrix:

```python
# order_flow.faults.star (auto-generated)
load("faultbox.star", "orders", "inventory", "order_flow")

inventory_down = fault_assumption("inventory_down",
    target = orders,
    connect = deny("ECONNREFUSED"),
)

inventory_slow = fault_assumption("inventory_slow",
    target = orders,
    connect = delay("500ms"),
)

fault_matrix(
    scenarios = [order_flow],
    faults = [inventory_down, inventory_slow],
)
```

Add `overrides=` to `fault_matrix()` for per-cell expectations. See
[CLI Reference](cli-reference.md) for all flags.

### `expect_success()` / `expect_error_within(ms)` / `expect_hang()` (v0.9.8)

Built-in outcome predicates for `default_expect=` and `overrides={}` in
`fault_matrix()`. They replace the hand-rolled assertion helpers every
mature spec grows ("is the result non-None? is the status under 500?"),
giving each matrix row an explicit, machine-readable outcome intent
that the v0.11.0 HTML report will consume (RFC-027, RFC-029).

```python
fault_matrix(
    scenarios = [get_config, health],
    faults    = [db_down, upstream_slow],
    default_expect = expect_success(),            # row passes → 200-ish, fast
    overrides = {
        (get_config, upstream_slow): expect_error_within(ms = 10000),
        (health, db_down):           expect_success(),  # health stays green
        # Deliberately trigger a client-timeout path.
        (get_config, db_down):       expect_hang(),
    },
)
```

Behaviour:

- `expect_success()` — scenario returned non-nil, `status_code` (if
  present) is `< 500`.
- `expect_error_within(ms=N)` — scenario returned with an error shape
  (`status_code >= 500` or non-empty `error` field) AND `duration_ms
  <= N`. "Returned 200 fast" violates this because the row was
  supposed to degrade.
- `expect_hang()` — scenario did not return at all (row timeout
  cancelled it). Used for deliberately exercising caller-timeout paths.

Backwards compatible: `default_expect=` still accepts plain Starlark
lambdas for rows that need custom checks.

#### Plan visualization (v0.13.0)

Every `fault_scenario` / `fault_matrix` declaration shows up as a node
in the plan tree. Run `faultbox plan <spec>` (RFC-042) to see the
test count and structure before launching: matrix axes are listed
explicitly, the total instance count is summarized, and a coverage
table flags dependency edges that no fault test targets.

```
$ faultbox plan tests/integration.star --coverage
Plan tree:
└── 2 tests
    ├── test "test_matrix_checkout"  [fault_matrix]
    │   ├── 4 instances
    │   └── fault_matrix
    │       ├── scenarios: [checkout]
    │       └── faults: [db_down, db_slow, redis_oom, kafka_down]
    └── test "test_smoke"  [def]

Coverage:
  ✓ api → db     (faulted in: test_matrix_checkout)
  ⚠ api → redis  (no fault test)
```

Useful for catching CI cost surprises before they fire. See
[docs/exploration.md](exploration.md) for the full plan / coverage /
`--suggest` / `--check-cost` surface.

#### Outcome taxonomy (v0.11.1)

Every `fault_scenario` / `fault_matrix` row produces one of five
outcomes in `manifest.json` and the HTML report:

| Outcome | Pill | Meaning |
|---|---|---|
| `passed` | green | Scenario returned; expect predicate (if any) accepted the result; any required faults fired. |
| `failed` | red | An `assert_*` inside the scenario body fired before the predicate ran. |
| `expectation_violated` | amber | Body assertions were clean, but the expect predicate disagreed with the result. |
| `fault_bypassed` | grey | Scenario returned cleanly, but a fault rule was installed and never matched a syscall (only with `require_faults_fire=True`). |
| `errored` | grey | Scenario raised an untyped error (crash, timeout outside a predicate). |
| `halted` | grey | RFC-043 `halt()` or `assume=` predicate pruned this branch. No verdict — counted separately from pass/fail. |
| `inconclusive` | grey | RFC-041 `eventually()` window timed out (or an `await_*` hit its timeout) without producing a verdict. Exit code 3. |

`expectation_violated` is a refinement of `failed` — legacy consumers
that only know the three-way taxonomy still see the row in the
`summary.failed` count. `fault_bypassed` is a refinement of `passed`
— the scenario did pass, but the test is uninformative because the
fault never fired. The predicate name (e.g. `expect_success`,
`expect_error_within`, or `lambda` for user callables) lands in
`manifest.tests[].expectation` and surfaces alongside the pill in the
report's tests table and drill-down header.

#### Per-leaf axis attribution (v0.13.0 rc2)

Multi-leaf rows from `choose()` / `probability=`/ `interleavings=`
fan-out carry their axis assignments on the manifest row:

- `leaf_choices` — map of named choose() axes to the **option index**
  (zero-based) the leaf selected. The HTML report's tests table
  renders this as `retries=2`, which means *"option at index 2"* —
  NOT the literal value. For `choose("retries", [0, 1, 3])`, leaf 2
  observes the value `3`; the display shows `retries=2`. The full
  spec (axis values) lives in `plan.json`'s recorded choices.
- `leaf_probability_outcomes` — per-rule fire/no-fire bit vector
  (`wal[10]` = first occurrence fires, second does not).
- `leaf_interleavings` — per `parallel()` call site, the
  ordering-index the leaf executed (`spec.star:7#3` = site at
  spec.star:7, ordering index 3).

The omitempty contract holds: single-leaf rows omit all three fields,
preserving the rc1 manifest shape.

#### `require_faults_fire=True` on `fault_matrix()` (v0.11.1)

Opt-in gate that demotes rows where at least one installed fault rule
never matched a syscall during the test:

```python
fault_matrix(
    scenarios           = [checkout, orders, health],
    faults              = [db_down, cache_slow],
    default_expect      = expect_success(),
    require_faults_fire = True,
)
```

Without the flag (default), a cell returning HTTP 200 goes green even
if the service cached an init-time response and never touched the
faulted upstream. With the flag on, such cells become
`fault_bypassed` (grey) and the drill-down lists every rule the
runtime saw unmatched — usually a hint that the scenario is hitting a
different code path than intended.

`require_faults_fire` composes with any `default_expect` /
`overrides` — the fault-fired check runs after the expect predicate.
Rows that already `failed` or `errored` keep their outcome.

### File readers — `load_file()`, `load_yaml()`, `load_json()` (v0.9.8)

Spec-load-time file reads. Use them instead of hand-inlining SQL
fixtures, OpenAPI specs, or JSON config as Starlark string constants.

```python
# Read raw bytes — returns a Starlark string.
seed_sql = load_file("./seed.sql")
mysql.exec(sql = seed_sql)

# Decode YAML into Starlark dict/list/scalar.
fixture = load_yaml("./fixtures/users.yaml")
for user in fixture["users"]:
    print(user["email"])

# Same shape for JSON. Integer-valued numbers become int, not float.
rates = load_json("./config/rates.json")
```

Path resolution is **relative to the spec file's directory** (same
base as `load()`), not the process `cwd`. Absolute paths work but
emit an INFO log line.

Security guardrails (see RFC-026 for rationale):

- Network schemes (`http://`, `https://`, `file://` with remote
  authority) refused with a clear error.
- Size cap: 50 MB per file by default. Override via
  `$FAULTBOX_LOAD_FILE_MAX_BYTES` (decimal bytes).
- `$FAULTBOX_HERMETIC=1` rejects symlinks escaping the spec dir.
- YAML non-string map keys refused (Starlark dicts need string keys).

Every file read via these builtins is **also captured into the `.fb`
bundle's `spec/` directory** so `faultbox inspect` and `faultbox
replay` see the exact source tree that produced the run. No separate
plumbing — piggybacks on the existing RFC-025 Phase 4 capture.

### `load(filename, symbol1, symbol2, ...)`

Imports symbols from another `.star` file. The loaded file shares the same
runtime (service registry, builtins, event log).

```python
# custom-failures.star
load("faultbox.star", "orders", "inventory", "order_flow")

inventory_down = fault_assumption("inventory_down",
    target = inventory,
    connect = deny("ECONNREFUSED"),
)

fault_scenario("order_inventory_down",
    scenario = order_flow,
    faults = inventory_down,
    expect = lambda r: assert_eq(r.status, 503),
)
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

### `fault_all([services], **syscall_faults, run=callback)`

Apply the same fault to multiple services simultaneously. Useful for testing
"all replicas down" or "entire dependency tier fails":

```python
# All three Kafka brokers down at once.
fault_all([kafka1, kafka2, kafka3],
    connect = deny("ECONNREFUSED"),
    run = scenario,
)

# All databases slow.
fault_all([pg_primary, pg_replica],
    write = delay("500ms"),
    run = scenario,
)
```

Equivalent to nesting `fault()` calls but without the lambda pyramid.
Faults are applied to all services before the callback runs, and removed
from all services after.

### `trace(service, syscalls=[...], run=callback)`

Observe syscalls without injecting faults. Installs seccomp filters that
record events but allow all syscalls to proceed normally.

```python
def test_observe_writes():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 200)
        assert_eventually(service="inventory", syscall="write", path="*.wal")
    trace(inventory, syscalls=["write", "openat", "fsync"], run=scenario)
```

Use `trace()` when you want to assert on internal behavior of a **healthy**
system — no faults, just observation.

### `trace_start(service, syscalls=[...])` / `trace_stop(service)`

Imperative trace control:

```python
def test_observe_then_fault():
    trace_start(inventory, syscalls=["write", "fsync"])
    resp = orders.post(path="/orders", body='...')
    assert_eventually(service="inventory", syscall="write", path="*.wal")
    trace_stop(inventory)
```

### `op(syscalls=[...], path=)`

Define a named operation that groups related syscalls. Used in `service()`
declarations with the `ops=` parameter.

```python
db = service("db", "./db",
    interface("main", "tcp", 5432),
    healthcheck=tcp("localhost:5432"),
    ops={
        "persist": op(syscalls=["write", "fsync"]),
        "wal_write": op(syscalls=["write", "fsync"], path="/tmp/*.wal"),
    },
)

def test_persist_failure():
    def scenario():
        resp = api.post(path="/data/key", body="val")
        assert_true(resp.status >= 500)
    fault(db, persist=deny("EIO"), run=scenario)
```

Named operations can include a **path filter** — only syscalls on matching
files are faulted. The trace shows the operation name: `persist(write) deny(EIO)`.

### `delay(duration, probability=, label=, max_fires=, mode=)`

Delays a syscall by sleeping before allowing it to proceed.

```python
delay("500ms")              # 500ms delay, 100% probability
delay("2s")                 # 2 second delay
delay("100ms", probability="50%")  # 50% chance of delay (stochastic)
delay("500ms", label="slow WAL")   # labeled for diagnostics
delay("100ms", probability=0.5, max_fires=2, mode="exhaustive")  # RFC-042 §8.9 fan-out
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `duration` | string | — | Go duration: `"500ms"`, `"2s"`, `"100us"` |
| `probability` | string or float | `"100%"` | Chance the fault fires. `"50%"` or `0.5` are equivalent. |
| `label` | string | — | Human-readable label shown in trace output |
| `max_fires` | int | — | RFC-042 §8.9 — exhaustive probability fan-out cap. Only with `probability < 1`; required when `mode="exhaustive"`. |
| `mode` | string | `"stochastic"` when `max_fires` unset; `"exhaustive"` when `max_fires` is set | `"exhaustive"` consults per-leaf vector across 2^max_fires leaves; `"stochastic"` is the legacy RNG path. Pass `mode="stochastic"` together with `max_fires=N` to reject the combo at spec load (incompatible). |

### `deny(errno, probability=, label=, max_fires=, mode=)`

Fails a syscall by returning an error code.

```python
deny("ECONNREFUSED")                     # 100% connection refused
deny("EIO", probability="10%")           # 10% I/O error (stochastic until max_fires= added)
deny("ENOSPC")                           # disk full
deny("EIO", label="WAL write")           # labeled for diagnostics

# RFC-042 §8.9 probability fan-out — exhaustive coverage of every
# fired/not-fired combination across N occurrences. 2^max_fires plan
# leaves; each leaf is a deterministic execution.
deny("EIO", probability=0.3, max_fires=3, label="wal", mode="exhaustive")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `errno` | string | — | Error code (see table below) |
| `probability` | string or float | `"100%"` | Chance the fault fires. `"50%"` or `0.5` are equivalent. |
| `label` | string | — | Human-readable label shown in trace output |
| `max_fires` | int | — | RFC-042 §8.9 — exhaustive probability fan-out cap. Only with `probability < 1`; required when `mode="exhaustive"`. Produces 2^N plan-tree leaves. |
| `mode` | string | `"stochastic"` when `max_fires` unset; `"exhaustive"` when `max_fires` is set | `"exhaustive"` consults the per-leaf vector across 2^max_fires leaves; `"stochastic"` keeps the legacy RNG-driven single realization. **Migration:** bare `probability=p` is unaffected by rc2 — you only opt into exhaustive fan-out by adding `max_fires=N`. |

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
| `http2` | `method=`, `path=` | response, error, delay, drop |
| `postgres` | `query=` (SQL-aware canonicalized match) | error, delay, drop |
| `mysql` | `query=` (SQL-aware canonicalized match) | error, delay, drop |
| `redis` | `command=`, `key=` | error, response, delay, drop |
| `grpc` | `method=` | error, delay, drop |
| `kafka` | `topic=` | drop, delay, error, duplicate |
| `mongodb` | `op=` / `method=` (cmd), `collection=` / `key=` | error, delay, drop |
| `cassandra` | `query=` (CQL) | error, delay, drop |
| `clickhouse` | `query=` (SQL, matches body or `?query=`) | error, delay, drop |
| `udp` | (datagram-level) | drop, delay |
| `amqp` | `topic=` (routing key) | drop, delay, error |
| `nats` | `topic=` (subject) | drop, delay |
| `memcached` | `command=`, `key=` | error, response, delay, drop |

### SQL query matching (v0.8.2+)

For the `postgres` and `mysql` proxies, `query=` patterns match incoming
SQL after both sides are run through a canonicalizer. This frees rule
authors from guessing exactly how a driver or ORM will format the query
on the wire:

- Case is folded (keywords lowercased; string-literal contents preserved).
- Whitespace runs collapse to single spaces; leading/trailing whitespace
  and trailing `;` are stripped.
- `?` and `$1`/`$2`/`$N` placeholders normalize to a shared `$?` marker,
  so a rule written with MySQL-style `?` matches a Postgres-style `$1`
  query and vice versa.
- `=`, `<`, `>`, `!=`, `<>`, `<=`, `>=`, `,`, `(`, `)` get space-padded
  so tight driver output (`"id=$1"`) matches user-written patterns
  (`"id = ?"`).
- Trailing `*` in the pattern remains a glob suffix (`INSERT*`).

A single rule pattern therefore matches every reasonable shape a driver
might emit:

```python
# This rule fires on every variant below:
rules = [mysql.deadlock(query = "UPDATE users SET role = ? WHERE id = ?")]

# ✓ "UPDATE users SET role = ? WHERE id = ?"
# ✓ "update users set role=$1 where id=$2"
# ✓ "UPDATE  users  SET role=$1 WHERE id=$2;"
```

### Trace events

Protocol proxy actions emit `type="proxy"` events into the trace:

```python
assert_eventually(where=lambda e: e.type == "proxy" and e.data.get("action") == "error")
```

### Importing recipes (standard library)

Faultbox ships a curated library of protocol-specific failure helpers
embedded in the binary. Load them via the `@faultbox/` prefix:

```python
load("@faultbox/recipes/mongodb.star",    "mongodb")
load("@faultbox/recipes/cassandra.star",  "cassandra")
load("@faultbox/recipes/clickhouse.star", "clickhouse")

broken = fault_assumption("broken",
    target = db.main,
    rules  = [
        mongodb.disk_full(collection = "orders"),
        cassandra.unavailable(),
        clickhouse.too_many_parts(),
    ],
)
```

Each recipe file exports one namespace struct named after the protocol
(see [RFC-018](rfcs/0018-recipes-library.md)). Zero name collisions
when you load recipes for multiple protocols — `mongodb.disk_full` and
`postgres.disk_full` coexist naturally.

Discover what's available:

```
$ faultbox recipes list
$ faultbox recipes show mongodb     # print a recipe's source
```

Recipes ship with the binary — no filesystem setup, no network fetch.
See [RFC-019](rfcs/0019-recipe-distribution.md) for the distribution
convention.

**User-authored recipes** work identically via relative paths:

```python
load("@faultbox/recipes/mongodb.star", "mongodb")   # stdlib
load("./recipes/checkout.star",        "checkout")  # your project

rules = [mongodb.disk_full(), checkout.post_q2_race()]
```

The `@faultbox/` prefix is reserved for the stdlib; everything else hits
the filesystem relative to the spec's directory.

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
matching `then` in the trace. Both `first=` and `then=` take **dicts** of
filter keys (same keys as `assert_eventually`: `service`, `syscall`, `path`,
`decision`, ...). Matching is over `syscall` events. Lambda predicates are
not accepted here, and there is no cross-event correlation between the
matched `first` and `then` events.

```python
# Dict matching:
assert_before(
    first={"service": "inventory", "syscall": "openat", "path": "/tmp/inventory.wal"},
    then={"service": "inventory", "syscall": "write", "path": "/tmp/inventory.wal"},
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

### `parallel(fn1, fn2, ..., interleavings=)`

Runs multiple step callables concurrently. Returns results in argument order.

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

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| positional | callable... | — | Two or more callables to run. |
| `interleavings` | int or string | `1` | RFC-042 §8.8 plan-tree fan-out policy. `1` (default) runs one interleaving; `"all"` fans out to N! leaves; `"critical"` to a 2N-1 heuristic subset; integer N caps to first N orderings. Reserved values (`"dpor"`, `"sut-internal"`) error explicitly. |

```python
# Fan out the plan tree across every ordering — N! test executions.
def test_all_interleavings():
    parallel(
        lambda: a.post("/op1"),
        lambda: b.post("/op2"),
        lambda: c.post("/op3"),
        interleavings = "all",   # 3! = 6 leaves
    )
```

**Scope limit (rc2):** today's "interleaving" is **launch ordering** — branches launch sequentially in the per-leaf order. Mediated-event-level ordering (two branches running concurrently with the engine releasing their syscalls in a specific sequence) is a follow-up; the kwarg surface and leaf descriptors are in place for that work to plug into.

```bash
faultbox test faultbox.star --runs 100 --show fail   # random interleavings (legacy)
faultbox test faultbox.star --explore=all             # exhaustive: ALL permutations
faultbox test faultbox.star --explore=sample           # 100 random orderings
faultbox test faultbox.star --seed 42                  # replay exact ordering
```

### `nondet(service, ...)` / `nondet()`

Two arities:

- `nondet(svc1, svc2, ...)` — excludes services from interleaving
  control during `parallel()`. Their syscalls proceed immediately
  without being held. Use this for services that make
  nondeterministic background requests (healthchecks, metrics).

  ```python
  def test_concurrent_orders():
      nondet(monitoring_svc, cache_svc)  # exclude from ordering exploration
      results = parallel(
          lambda: orders.post(path="/orders", body='...'),
          lambda: orders.post(path="/orders", body='...'),
      )
  ```

- `nondet()` (zero-arg) — RFC-043 §5.1 non-deterministic boolean.
  Sugar for `choose([True, False])`. rc1 returns the first option
  (`True`); rc2 fans the plan tree into two leaves.

### `choose(options)` / `choose(name, options)` (v0.13.0)

Finite non-deterministic choice (RFC-043 §5.2). Returns the first
option in rc1; the plan tree records the call site and option set so
rc2 can produce one leaf per option.

```python
retries = choose([0, 1, 3])              # anonymous
retries = choose("retries", [0, 1, 3])   # named — visible to assume()
```

See [docs/nondeterministic-operators.md](nondeterministic-operators.md).

### `halt(reason="")` (v0.13.0)

Prune the current plan-tree branch (RFC-043 §5.3). Body execution
stops; the test outcome is `"halted"` — distinct from
pass/fail/inconclusive. Only valid inside a test body.

```python
def body():
    retries = choose([0, 1, 3])
    if retries == 0 and nondet():
        halt("uninteresting branch")
    api.run_workflow(retries=retries)
```

### `assume(predicate)` (v0.13.0)

Filter the plan tree (RFC-043 §5.4). Top-level form evaluates at
spec load; the `test(assume=[...])` kwarg evaluates per-test at body
entry. Predicate receives a `choices` dict mapping each named
`choose("name", opts)` call to its currently-selected option.

```python
assume(lambda choices: choices["retries"] < 5)

test("guarded",
    body = body,
    assume = [lambda choices: choices["fault"] != "timeout"],
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

## Determinism (RFC-040)

Faultbox v0.13.0 makes **L1 determinism** a contract: every spec runs at L1 by default and the runtime detects unmediated I/O the SUT performs (`clock_gettime`, `getrandom`, DNS, connect to undeclared destinations) and emits `unmediated_io` events. Strict mode (the default) fails the test on the first such event whose category isn't tolerated; non-strict mode records them as warnings only.

The full taxonomy (L0 plan determinism through L5 instruction-boundary, why the levels split where they do, and the post-L1 roadmap) lives in [docs/determinism.md](determinism.md). This section is the spec-language reference for the two builtins.

### `determinism(level=, runtime=, strict=, allow=)`

Top-level declaration. May be called at most once per spec; if omitted, the spec runs with the L1 / `default` runtime / `strict=True` defaults.

| Kwarg | Type | Default | Notes |
|-------|------|---------|-------|
| `level` | string | `"L1"` | One of `"L0"`, `"L1"`. `"L2"`–`"L5"` parse but error at spec load (reserved for future releases — see [RFC-046](rfcs/0046-beyond-l1-roadmap.md)). |
| `runtime` | string | `"default"` | The substrate. v0.13.0 ships only `"default"` (seccomp-notify). `"gvisor"` parses but errors. |
| `strict` | bool | `True` (at L1) | `True` ⇒ untolerated `unmediated_io` events fail the test. `False` ⇒ they appear as warnings. Passing `strict=` at L0 is rejected — L0 has no detection events, so the kwarg can't honor any intent. |
| `allow` | list | `[]` | Spec-wide tolerated categories. Items must come from `clock`, `rand`, `dns`, `network-unmediated`, `fs-unmediated`. Any other string is rejected at spec load. |

CLI overrides at runtime:

```
faultbox test --strict-determinism              # force on
faultbox test --strict-determinism=true         # same
faultbox test --strict-determinism=false        # force off
faultbox test --no-strict-determinism           # alias for =false
```

Override is bidirectional and final — beats whatever the spec declared. Useful for iterating locally on a strict CI spec without editing it.

```python
# Default behaviour: L1 / default runtime / strict on, no escape hatches.
service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
)

# Spec-wide tolerance for clock + DNS drift across every service.
determinism(allow = ["clock", "dns"])

# Loosen for local dev iteration only.
determinism(level = "L1", strict = False)
```

#### Feature interactions — what caps below L4 (RFC-044 §8.5)

Some service-level features bound the achievable determinism level even when the spec asks for higher. These caps are documented here so spec authors don't reach for `level="L4"` only to discover at run time that their topology can't reach it.

| Feature | Max level achievable | Why |
|---|---|---|
| `service(remote=...)` (RFC-036) | **L1** | Faultbox does not own the remote process, so unmediated I/O on the remote side is invisible to the runtime. Strict mode still gates the local code path; the remote stays best-effort. |
| `service(reuse=True, …)` (RFC-015) | **L1** | Container state carried across tests breaks the per-test determinism contract that L2+ requires (seed-only reset isn't equivalent to a fresh process). Use the default `reuse=False` for L2+ specs. |

L4 and L5 levels are reserved (`docs/determinism.md`) — for now any spec declaring them errors at spec load. When L4 lands, `service(remote=...)` and `service(reuse=True, ...)` will be rejected at that level with an error pointing to the documented cap above.

**`mock_service(...)` (RFC-017) does not cap the determinism ceiling.** Mock services are in-process stubs with no separate process to mediate, so they're compatible with every level the spec declares. They're listed here for completeness but stay outside the cap table.

### Per-service tolerance: `nondeterministic_ok=`

`service()` accepts a `nondeterministic_ok = [...]` kwarg listing categories tolerated *for that service alone*. The runtime takes the union of `determinism(allow=...)` and `service.nondeterministic_ok` when deciding whether to fail.

```python
determinism(allow = ["clock"])           # everyone is allowed clock drift

service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
    nondeterministic_ok = ["dns"],       # api also tolerated DNS
)

service("worker", "/usr/local/bin/worker",
    interface("main", "http", 8081),
    # no escape hatches — clock allowed (from spec), DNS would fail
)
```

### `unmediated_io` event schema

Every detected leak emits one `unmediated_io` event. Stable fields the report and tooling can rely on:

| Field | Always present | Description |
|-------|---------------|-------------|
| `category` | yes | One of `clock`, `rand`, `dns`, `network-unmediated`, `fs-unmediated` |
| `syscall` | yes | The syscall that triggered the event (e.g. `clock_gettime`, `connect`) |
| `pid` | yes | Process ID inside the SUT |
| `detail` | sometimes | For network categories: `host:port` of the unmediated destination |

When strict mode fires, the test outcome is `strict_determinism_violation` (a refinement of `failed`, similar to how `expectation_violated` refines `failed` and `fault_bypassed` refines `passed`). The bundle's `Summary.StrictDeterminismViolation` counts these rows; legacy `Summary.Failed` also counts them so existing CI gates keep working.

### What L1 detection actually catches

| Category | Detection mechanism | Caveat |
|----------|--------------------|--------|
| `clock` | `clock_gettime` syscall | **VDSO unreachable** — Go's `time.Now()` uses VDSO and seccomp cannot intercept it. Best-effort detection. |
| `rand` | `getrandom` syscall + `/dev/urandom` reads | Catches the kernel-call paths; an in-process Mersenne twister seeded once at startup is invisible. |
| `dns` | `connect()` whose destination port is `53` outside any declared interface | Misses DoH (`https://*/dns-query`) and DoT (port 853). |
| `network-unmediated` | `connect()` to an address/port not matching any declared `interface()` and not bound by a Faultbox proxy | The cleanest signal; works for any TCP destination. |
| `fs-unmediated` | reserved category — accepted in `allow=` lists but emits no events in v0.13.0 | Detection lands in a future release; the kwarg is reserved so future migration is non-breaking. |

Detection only kicks in for services that already need a seccomp filter (because some `fault()` rule targets them). Unfaulted services keep their native-speed path — there's no event log path for them anyway. To enable detection without injecting any actual fault, declare a `fault()` rule with no faults (the runtime still installs the filter for the L1 categories).

---

## Monitors

### `monitor(name, on=, state_init=, update=, check=) → MonitorDef`

A monitor is a spec-wide observer that fires on matching events
throughout a test, maintaining per-test memory via a small state
machine. Replaces the legacy callback-style monitor (RFC-041 §5.4).

**Required:** `name` (positional), `on=` (a `MatcherVal` from
`match.event/any/all`). **Optional:** `state_init` (any Starlark value
seeded fresh per test, default `None`), `update` (`lambda event,
state: new_state`, default identity), `check` (`lambda event, state:
bool`, default always-pass).

For each event satisfying `on=`:
1. `new_state = update(event, state)`
2. `verdict = check(event, new_state)`
3. If `verdict` is false → test FAILs, citing the event
4. `state ← new_state` for the next iteration

```python
monitor("balance_invariant",
    on         = match.event(type="account.balance"),
    state_init = {"last_balance": None},
    update     = lambda event, state: {"last_balance": event.amount},
    check      = lambda event, state:
                     state["last_balance"] == None or state["last_balance"] >= 0,
)
```

**Scoping** — a top-level `monitor(...)` auto-registers spec-wide
(fires for every test). A monitor passed to
`fault_assumption(monitors=)`, `fault_scenario(monitors=)`, or
`fault_matrix(monitors=)` is scenario-scoped instead; Faultbox claims
it from the spec-wide list so it doesn't double-register.

**Sandbox restrictions** — `update` and `check` lambdas run in a
restricted Starlark thread. Calls to `fault`, `service`, `assert_*`,
`parallel`, `partition`, `eventually`, `always`, `monitor`,
`await_stable`, `await_event`, `determinism`, `trace`, `trace_start`,
`trace_stop`, `events`, and friends are rejected at spec load with a
clear error message. See
[docs/temporal.md](temporal.md#sandbox-restrictions) for the full
list.

---

## Temporal Primitives (RFC-041)

The temporal primitives express *what must be true* about a
distributed system rather than *how long to wait* before checking.
See [docs/temporal.md](temporal.md) for the full guide; the entries
below are the spec-language reference.

### `eventually(predicate, anchor=)`

Asserts that `predicate` holds at some point before Termination. No
per-assertion deadline — only the test `timeout=` bounds it.

```python
test("order_propagates",
    body   = lambda: api.create_order(sku="abc", qty=5),
    expect = eventually(
        lambda t: t.event(type="inventory.stock", sku="abc").qty == -5,
    ),
    timeout = "30s",
)
```

Optional `anchor=<matcher>` narrows the relevant event range so
evaluation doesn't start until the anchor event fires.

### `always(predicate, between=)`

Invariant: `predicate` must hold for every evaluation between two
anchors. A single false evaluation fails immediately.

```python
expect = always(
    lambda t: t.event(type="balance").amount >= 0,
    between = ("body_start", "stable"),
)
```

`between=` accepts a tuple of two anchors: each is either a string
lifecycle marker (`"body_start"`, `"body_end"`, `"stable"`) or a
`MatcherVal`.

### `await_event(predicate_or_matcher)`

Blocks the test body until the event log contains a matching event.
Eager-checks on entry; returns the matching event.

```python
def body():
    api.start_workflow(id="wf-42")
    event = await_event(match.event(type="workflow.phase_1", id="wf-42"))
    api.kick_off_phase_2(id=event.id)
```

No own timeout — bounded by `test(timeout=)`.
Reserved kwarg `clock="virtual"` errors with a gVisor migration message.

### `await_stable(quiescence_window=, ignore=)`

Blocks the body until no non-ignored event has fired for the full
quiescence window. Default `quiescence_window="1s"`.

```python
def body():
    api.start_workflow()
    await_stable(quiescence_window="500ms",
                 ignore=match.event(type="heartbeat"))
    api.check_status()
```

`ignore=` accepts a matcher or callable for events that should not
reset the quiescence timer (heartbeats, telemetry, metric flushes).

### `test(name, body=, setup=, expect=, timeout=, terminate_when=, assume=, clock=)`

Declarative test wrapper with explicit temporal config. Registered as
`test_<name>` so CLI `--test foo` and `--test test_foo` both work.

```python
test("eventual_propagation",
    body            = lambda: api.start_workflow(id="wf-42"),
    timeout         = "30s",
    terminate_when  = eventually(
        lambda t: t.event(type="workflow.status", id="wf-42").status == "completed"
    ),
    expect          = always(lambda t: t.event(type="balance").amount >= 0),
)
```

Legacy `def test_*()` functions remain supported. They use the same
lifecycle without per-test timeout or `terminate_when=`.

### Termination & three-valued verdict

A test terminates when the first of these conditions fires:

- **(a) Natural completion** — body returned **and** every
  registered `eventually`/`always` is in a positive terminal state
- **(b) `terminate_when=` predicate fires**
- **(c) `timeout=` deadline elapsed**
- **(d) Immediate failure** — body raised, `always` violated mid-test,
  or a monitor fired

Verdict per cause:

| Cause | All eventually satisfied | Some unsatisfied |
|-------|--------------------------|------------------|
| (a) Natural | PASS | FAIL |
| (b) terminate_when | PASS | FAIL |
| (c) timeout | PASS | **INCONCLUSIVE** |
| (d) Immediate failure | n/a | FAIL |

CLI exit code: `0` pass, `2` any-fail, `3` inconclusive-only.

### The `match` module

| Constructor | Meaning |
|-------------|---------|
| `match.event(type=..., **fields)` | Single-event matcher (field globs supported) |
| `match.any(*matchers)` | OR composition |
| `match.all(*matchers)` | AND composition |
| `match.never()` | Never matches |

### The trace API (in predicates)

Predicates receive a `trace` object. Most-used operators:

- `trace.event(type=..., **fields)` → most recent (last) matching `EventVal`, or `None` (alias for `trace.last(...)`; use `trace.first(...)` for the earliest)
- `trace.events(matcher)` → wrapped sequence with `.map/.filter/.reduce`
- `trace.first(matcher)` / `trace.last(matcher)` / `trace.count(matcher)`
- `trace.events_between(start, end)` / `trace.events_within(matcher, window, of=event)`
- `trace.causal_chain(event)`

`EventVal` exposes `.type`, `.service`, `.seq`, `.timestamp`, plus
causal methods (`happens_before/after`, `concurrent_with`,
`same_service_as`, `preceded_by/within`, `followed_by/within`,
`directly_caused_by`, `duration_since`).

### `duration(str) → int`

Parses a duration string (`"200ms"`, `"1.5s"`, `"2m"`) into integer
**nanoseconds**. `event.duration_since(other)` returns the same units, so
the two compose with numeric comparisons (`<`, `<=`, `>`, `>=`).

```python
# Assert two events happened within 200ms of each other:
eventually(lambda t:
    t.event(type="response").duration_since(t.event(type="request")) < duration("200ms"))
```

---

## Fault Composition

The fault composition builtins separate **what the system does** (scenario),
**what goes wrong** (fault assumption), and **what correct means** (expect oracle).

### `fault_assumption(name, target=, **syscall_faults, rules=, monitors=, faults=, description=)`

Creates a named, reusable fault configuration. Returns a `FaultAssumption`
value that can be stored in variables and passed to `fault_scenario()`,
`fault_matrix()`, or `fault()`.

```python
# Syscall-level fault: deny connections to inventory.
inventory_down = fault_assumption("inventory_down",
    target = inventory,
    connect = deny("ECONNREFUSED"),
)

# Syscall-level fault: disk full on inventory WAL writes.
disk_full = fault_assumption("disk_full",
    target = inventory,
    write = deny("ENOSPC"),
)

# Latency fault on the order service network.
slow_network = fault_assumption("slow_network",
    target = orders,
    connect = delay("200ms"),
    write = delay("100ms"),
)
```

**Syscall kwargs** resolve in the same order as `fault()`:

1. Named operation on `target.ops` → expands to the op's syscalls + path glob
2. Syscall family name → expands via family (e.g., `write` → write, writev, pwrite64)
3. Raw syscall name → used as-is

**Named operations:**

```python
inventory = service("inventory", "/tmp/inventory-svc",
    interface("main", "tcp", 5432),
    ops = {"persist": op(syscalls=["write", "fsync"], path="/tmp/*.wal")},
)

wal_corrupt = fault_assumption("wal_corrupt",
    target = inventory,
    persist = deny("EIO"),  # expands to write+fsync on /tmp/*.wal
)
```

**Protocol-level faults** (when target is an interface reference):

```python
pg_insert_fail = fault_assumption("pg_insert_fail",
    target = postgres.main,
    rules = [error(query="INSERT*", message="disk full")],
)
```

**Composition** — combine multiple assumptions into one:

```python
cascade = fault_assumption("cascade",
    faults = [inventory_down, slow_network],
    description = "Inventory unreachable AND slow network",
)
# cascade inherits all rules and monitors from both children.
```

**With monitors:**

```python
# check= returns False on the first matching event => the test FAILs,
# citing that event ("this must never happen").
no_traffic = monitor("no_traffic",
    on = match.event(type="syscall", service="inventory", syscall="read"),
    check = lambda event, state: False,
)

inventory_down = fault_assumption("inventory_down",
    target = inventory,
    connect = deny("ECONNREFUSED"),
    monitors = [no_traffic],  # active whenever this assumption is applied
)
```

**Using with `fault()`** directly:

```python
def test_order_down():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
    fault(inventory_down, run=scenario)
```

### `fault_scenario(name, scenario=, faults=, expect=, monitors=, timeout=)`

Composes a scenario probe with fault assumptions and an expect oracle.
Registers as `test_<name>`.

```python
# Basic: scenario + fault + oracle.
fault_scenario("order_inventory_down",
    scenario = order_flow,
    faults = inventory_down,
    expect = lambda r: (
        assert_eq(r.status, 503),
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal"),
    ),
)

# Multiple faults applied simultaneously.
fault_scenario("order_cascade",
    scenario = order_flow,
    faults = [inventory_down, slow_network],
    expect = lambda r: assert_true(r.status >= 500),
)

# Smoke test — no expect, just "must not crash".
fault_scenario("order_disk_full_smoke",
    scenario = order_lifecycle,
    faults = disk_full,
)

# With scenario-level monitor and custom timeout.
fault_scenario("order_retries",
    scenario = order_flow,
    faults = inventory_down,
    monitors = [retry_monitor],
    expect = lambda r: assert_eq(r.status, 503),
    timeout = "10s",
)
```

**Execution model:**

1. Register monitors (from fault assumptions + scenario-level)
2. Apply fault rules from all assumptions
3. Run the scenario function, capture its return value
4. If any monitor fired a violation → test fails (expect not called)
5. Call `expect(return_value)` — expect validates via `assert_*` side-effects
6. Remove faults and monitors

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `name` | string | Test name → registered as `test_<name>` |
| `scenario` | callable | The probe function (should return observable) |
| `faults` | `FaultAssumption` or list | Fault(s) to apply |
| `expect` | callable or None | Oracle: `(result) → void`, calls `assert_*` to validate |
| `monitors` | list of `MonitorDef` | Scenario-level invariants |
| `timeout` | string | Max duration (default `"30s"`) |

### `fault_matrix(scenarios=, faults=, default_expect=, overrides={}, monitors=[], exclude=[])`

Generates the cross-product of scenarios × fault assumptions. Each cell
becomes a `fault_scenario` registered as `test_matrix_<scenario>_<fault>`.

```python
fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [inventory_down, disk_full, slow_network],
    default_expect = lambda r: assert_true(r != None, "must return a response"),
    overrides = {
        (order_flow, inventory_down): lambda r: (
            assert_eq(r.status, 503),
            assert_true("unreachable" in r.body),
        ),
        (order_flow, slow_network): lambda r: (
            assert_eq(r.status, 200),
            assert_true(r.duration_ms > 100),
        ),
        (health_check, inventory_down): lambda r: assert_eq(r.status, 503),
    },
    exclude = [
        (health_check, disk_full),  # health check doesn't touch disk
    ],
)
# Generates 5 tests: 2×3 - 1 excluded
```

**Override precedence:** cell-specific override > default\_expect > None (smoke test).

**Matrix report** — when matrix tests run, the terminal shows a summary table:

```
Fault Matrix: 2 scenarios × 3 faults = 5 cells

                    │ inventory_down │ disk_full     │ slow_network
────────────────────┼────────────────┼───────────────┼──────────────
order_flow          │ PASS (12ms)    │ PASS (8ms)    │ PASS (310ms)
health_check        │ PASS (5ms)     │ — (excluded)  │ PASS (205ms)

Result: 5/5 passed
```

JSON output (`--format json`) includes a `"matrix"` section with scenarios,
faults, cells, and pass/fail counts.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `scenarios` | list of callables | Scenario probe functions |
| `faults` | list of `FaultAssumption` | Fault assumptions |
| `default_expect` | callable or None | Default oracle for cells without overrides |
| `overrides` | dict | `(scenario, fault)` tuple → cell-specific expect |
| `monitors` | list of `MonitorDef` | Matrix-wide invariants (all cells) |
| `exclude` | list of tuples | `(scenario, fault)` pairs to skip |

---

## Data Integrity Verification

The `expect` oracle in `fault_scenario()` and `fault_matrix()` can use
**protocol steps** to query service state directly — not just check the
HTTP response. This is how you verify data integrity after fault injection.

### Querying the database in expect

After a fault scenario, ask the database whether the data is correct:

```python
def create_order():
    return api.public.post(path="/orders", body='{"item":"widget","qty":1}')

scenario(create_order)

db_write_error = fault_assumption("db_write_error",
    target = db,
    write = deny("EIO"),
)

fault_scenario("no_partial_rows_on_error",
    scenario = create_order,
    faults = db_write_error,
    expect = lambda r: (
        # 1. API returned an error
        assert_true(r.status >= 500, "should fail on DB write error"),

        # 2. No orphaned rows in the database
        assert_eq(
            db.main.query(sql="SELECT count(*) as n FROM orders WHERE status='pending'").data[0]["n"],
            0,
            "no partial rows should exist after failed INSERT"),

        # 3. The fault actually fired
        assert_eventually(type="syscall", service="db", decision="deny*"),
    ),
)
```

The key: `db.main.query(sql=...)` is a protocol step — it talks to the
running database over the wire. Inside `expect`, the service is still
running, so you can query its actual state.

### Querying Redis in expect

Verify cache state after a fault:

```python
redis_down = fault_assumption("redis_down",
    target = api,
    connect = deny("ECONNREFUSED"),
)

fault_scenario("no_stale_cache_after_failure",
    scenario = create_order,
    faults = redis_down,
    expect = lambda r: (
        assert_true(r.status >= 500),

        # No stale cache entries should remain
        assert_eq(
            len(redis.main.keys(pattern="order:*").data),
            0,
            "no cached order keys after Redis failure"),
    ),
)
```

### Verifying Kafka message integrity

Use event sources (`observe=`) to track produced and consumed messages,
then verify in `expect`:

The `topic` event source (Kafka/NATS messages) is a Go event-source
plugin; it has no `observe.*` Starlark constructor yet, so it can't be
attached inline like `observe.stdout`. The queries below show how you
assert over `"topic"`-typed events once that source is wired. To capture
produce/consume activity today, attach `observe.stdout` /
`observe.stderr` to the SUT and match its own structured log lines.

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    healthcheck = tcp("localhost:9092"),
)

fault_scenario("no_message_loss_on_db_error",
    scenario = create_order,
    faults = db_write_error,
    expect = lambda r: (
        assert_true(r.status >= 500),

        # No Kafka events should be published if DB write failed
        assert_never(where=lambda e:
            e.type == "topic" and e.data.get("topic") == "order-events"),
    ),
)
```

For "all produced messages were consumed" (no message loss):

```python
fault_scenario("consumer_catches_up",
    scenario = publish_and_consume,
    faults = consumer_slow,
    expect = lambda r: (
        assert_eq(
            len(events(where=lambda e: e.type == "topic"
                and e.data.get("action") == "produce")),
            len(events(where=lambda e: e.type == "topic"
                and e.data.get("action") == "consume")),
            "every produced message must be consumed"),
    ),
)
```

### Verifying proxy-injected errors

When using protocol-level faults (via `rules=`), the proxy logs every
injected error as a `type="proxy"` event. Verify the fault actually
fired:

```python
db_insert_fail = fault_assumption("db_insert_fail",
    target = db.main,
    rules = [error(query="INSERT*", message="disk full")],
)

fault_scenario("insert_rejected_by_proxy",
    scenario = create_order,
    faults = db_insert_fail,
    expect = lambda r: (
        assert_true(r.status >= 500),

        # Verify the proxy intercepted and rejected the INSERT
        assert_eventually(type="proxy", where=lambda e:
            "INSERT" in e.data.get("query", "")
            and e.data.get("action") == "error"),
    ),
)
```

### Monitor pattern: continuous data integrity

For invariants that must hold across ALL scenarios and faults — not just
one specific test — use monitors on fault assumptions:

```python
# A monitor's update/check lambdas run sandboxed - no queries, no fail().
# Express "no publish without a committed row" by counting the two observed
# streams and checking the relation (published must not outrun committed).
orphan_check = monitor("no_orphan_events",
    on = match.any(match.event(type="topic"), match.event(type="wal")),
    state_init = {"published": 0, "committed": 0},
    update = lambda event, state: {
        "published": state["published"] + (1 if event.type == "topic" else 0),
        "committed": state["committed"] + (
            1 if event.type == "wal" and event.data.get("op") == "INSERT" else 0),
    },
    check = lambda event, state: state["published"] <= state["committed"],
)

# Attach to every fault that could cause this inconsistency
db_write_error = fault_assumption("db_write_error",
    target = db,
    write = deny("EIO"),
    monitors = [orphan_check],
)
```

### Summary: which tool for which check

| What you want to verify | Tool | Example |
|------------------------|------|---------|
| HTTP response status/body | `expect` lambda on `r` | `assert_eq(r.status, 503)` |
| Database row exists/absent | `db.main.query(sql=...)` in `expect` | `assert_eq(row_count, 0)` |
| Redis key exists/absent | `redis.main.keys(pattern=...)` in `expect` | `assert_eq(len(keys), 0)` |
| Kafka message published/absent | `assert_eventually`/`assert_never` on events | `assert_never(type="topic", ...)` |
| Proxy-injected error fired | `assert_eventually(type="proxy", ...)` | Verify INSERT was rejected |
| Fault actually fired | `assert_eventually(decision="deny*")` | Avoid silent test pass |
| Continuous invariant across all tests | `monitor()` on `fault_assumption` | "no orphan events" |
| Message loss / consumer lag | Compare `events()` counts in `expect` | produced count == consumed count |

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
| `proxy_conn_open` | Transparent proxy accepted client + dialed upstream (RFC-034) |
| `proxy_conn_close` | Proxy connection terminated; carries `bytes_c2s` / `bytes_s2c` / `duration_ms` / `reason` |
| `proxy_handshake_complete` | Protocol-aware proxy finished its auth/handshake phase (mysql, postgres, redis) |
| `proxy_stall` | Proxy direction blocked on pending bytes for ≥ stall threshold (default 5s warn, 30s extend) |
| `stdout` | Service stdout line (when `observe=[observe.stdout(...)]`) |
| `stderr` | Service stderr line (when `observe=[observe.stderr(...)]`) |

The `partition_key` field (default: service name) enables routing events to
per-service PObserve monitor instances.

**`step_recv` fields.** Each `step_recv.<service>` event carries
`status_code`, `duration_ms`, `success`, and a human `summary`, plus any
protocol-specific fields (e.g. `rows`, `collection`). On a **non-2xx HTTP
response** the event also carries `body` — the response body, truncated to
2 KB (since v0.13.0). This lets you read why a 400/500 happened straight
from the trace or HTML report instead of re-running with the body inlined
into an assert message. 2xx bodies are omitted to keep bundles small; the
full, untruncated body is always available on the in-test
[`Response`](#type-response) object (`resp.body`).

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
faultbox init --from-compose docker-compose.yml      # generate from compose
faultbox init --claude                                # Claude Code integration
faultbox init --vscode                                # VS Code autocomplete

# Generate failure scenarios
faultbox generate faultbox.star                       # per-scenario fault files
faultbox generate faultbox.star --dry-run             # preview without writing

# Structured output (for LLM agents / CI)
faultbox test faultbox.star --format json             # JSON to stdout

# MCP server (for Claude Code, Cursor, etc.)
faultbox mcp                                          # start MCP server on stdio

# Maintenance
faultbox self-update                                  # update to latest release
faultbox --version                                    # print version
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
