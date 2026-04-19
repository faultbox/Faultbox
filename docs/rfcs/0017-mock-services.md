# RFC-017: Native Mock Services — Stub Protocols Without Containers

- **Status:** Implemented (v0.8.0)
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-04-17
- **Amended:** 2026-04-19 — split generic `mock_service()` from protocol-specific stdlib mocks; promoted dynamic handlers + TLS into v1; spun SQL mocks off to RFC-020 (#39) and OpenAPI generation to RFC-021 (#40)
- **Shipped:** 2026-04-19 (v0.8.0, PRs #41–#48)
- **Branch:** `epic/v0.8-mock-services`
- **Milestone:** v0.8.0
- **Addresses:** FB-017

## Summary

Add a `mock_service()` builtin that stands up **in-process protocol stubs**
defined purely in Starlark — no container, no binary, no Dockerfile. The stub
answers traffic on the declared interface, emits events into the Faultbox
event log like any real service, and remains subject to `fault()` rules.

First-class protocols at ship: **HTTP, HTTP/2, TCP, UDP, gRPC, Redis,
Kafka, MongoDB**. Follow-on protocols (Postgres, MySQL, NATS, Cassandra,
ClickHouse) layer on after the core lands.

Scope note: RFC-016 shipped MongoDB, HTTP/2, UDP, Cassandra, and
ClickHouse as *fault-injected real-service* protocols. Those same
protocols are candidates for mocking — wherever a real service can be
faulted, a mock can usually stand in. The phasing below reflects the
updated protocol surface.

## Motivation

### The JWKS / auth stub problem

Real specs routinely need a small always-on dependency — an OIDC JWKS
endpoint, a feature-flag service, a metadata endpoint, a license server —
whose behavior is trivial (a constant JSON document, a 200 OK, a tiny
lookup table) but whose absence blocks the system under test from
starting.

Today users have three options, all bad:

1. **Custom Docker image.** Write a Dockerfile, build it, ship it, keep it
   in sync. Overkill for 6 lines of JSON.
2. **`python:3-alpine` + `http.server` + volume mount.** Works but imports
   Python into the dependency graph, fights volume-mount semantics on
   macOS/Lima, still boots a container.
3. **Disable the code path.** Add conditional compilation or env-var
   toggles to the SUT itself — changes production code to make tests pass.
   A smell.

All three have the same problem: **the stub behavior is declarative data,
but the deployment is procedural containers.** Faultbox already has a
declarative spec language. It should be able to express "this endpoint
returns this JSON" directly.

### Why this is *not* `response()`

The existing [`response()`](../spec-language.md) builtin is a
**fault-injection primitive** — it rewrites responses from an *existing*
upstream service inside `fault()`. It presupposes a real server on the
other side that sometimes gets overridden. Mock services have no upstream:
they *are* the server.

### Beyond HTTP

Auth stubs are just the loudest example. The same pattern applies across
protocols:

| Use case | Protocol | What the stub does |
|---|---|---|
| JWKS / OIDC metadata | HTTP | Return fixed JSON document |
| Feature flags | HTTP / gRPC | Return flag map from in-memory dict |
| Metrics sink | UDP | Accept StatsD datagrams, count them |
| License validator | gRPC | Return `valid=true` for all requests |
| Config store | Redis | Answer `GET config:*` from a map |
| Outbound events | Kafka | Accept produces, expose them as events |
| Legacy TCP service | TCP | Echo or respond to framed requests |

A protocol-aware mock primitive gives all of these the same treatment.

## Design

### Two-tier API — generic primitive + protocol stdlib

The mock surface splits cleanly along protocol semantics:

1. **Request/response protocols** (HTTP, HTTP/2, TCP, UDP, gRPC) get the
   generic `mock_service()` builtin with a `routes={}` map. These protocols
   all reduce to "match incoming request, return response," so one primitive
   covers them.
2. **Stateful or message-broker protocols** (Kafka, Redis, MongoDB,
   Cassandra) ship as **protocol-specific stdlib modules** under
   `@faultbox/mocks/<protocol>.star`, reusing the embedded-stdlib
   distribution pattern from RFC-019. Each module exports a struct with a
   constructor tailored to that protocol's shape (topics for Kafka,
   key-value state for Redis, collections for MongoDB).

This split fixes a leak in the original RFC draft: shoving `topics=`,
`state=`, `collections=` onto a single generic builtin would force
`mock_service()` to grow a new protocol-specific kwarg per new protocol.
The stdlib-module pattern keeps `mock_service()` truly generic and puts
protocol invariants where they belong.

### The generic builtin

```python
mock_service(name, *interfaces,
    routes:   dict = {},      # "METHOD /path" (HTTP/HTTP2) | bytes prefix (TCP/UDP) | "/pkg.Service/Method" (gRPC)
    default:  MockResponse = None,  # fallback for unmatched requests (default: 404)
    tls:      bool = False,   # self-signed cert; CA bundle mounted into containers
    depends_on: list = [],    # same semantics as service()
    # Fault/reuse/seed/reset are NOT supported — mocks are always reusable
    # and stateless-by-default.
)
```

- Returns a `MockServiceRef` with the same `.interface` attribute shape as
  `service()`, so downstream builtins (`fault()`, `events()`, assertions)
  treat it identically.
- The runtime starts an in-process listener on the declared port for each
  interface. No Docker, no shim, no seccomp.
- Every request is **logged as an event** — same schema as intercepted
  syscalls, so assertions like `assert_eventually(events().where(service =
  "auth-stub", op = "GET /jwks").count() > 0)` work uniformly.
- Routes accept `dynamic(fn)` for per-request Starlark callbacks (JWT
  signing, dynamic claims, etc.) — see "Dynamic handlers" below.

### Protocol stdlib modules

Shipped embedded in the `faultbox` binary and loaded via the `@faultbox/`
prefix (RFC-019 distribution):

```python
load("@faultbox/mocks/kafka.star",   "kafka")
load("@faultbox/mocks/redis.star",   "redis")
load("@faultbox/mocks/mongodb.star", "mongo")

bus   = kafka.broker(name = "bus",   interface = interface("main", "kafka", 9092),
                     topics = {"orders": [...], "payments": []})

cache = redis.server(name = "cache", interface = interface("main", "redis", 6379),
                     state = {"config:max_retries": "3"})

users = mongo.server(name = "users", interface = interface("main", "mongodb", 27017),
                     collections = {"users": [...]})
```

Each module exports one struct named after the protocol (same pattern as
RFC-018 recipes). Constructors encode protocol-specific invariants:
`kafka.broker` requires `topics=`, `redis.server` accepts `state=`,
`mongo.server` accepts `collections=`.

Under the hood, each stdlib constructor is a thin wrapper that calls the
same Go `MockHandler` machinery — the split is purely at the Starlark
surface for clean semantics, not two separate runtime paths.

### Response constructors

Protocol-agnostic factories that produce typed response values:

```python
json_response(status, body, headers={})   # HTTP, HTTP/2, gRPC JSON
text_response(status, body, headers={})   # HTTP, HTTP/2 text/plain
bytes_response(status, data)              # Raw bytes (TCP, UDP)
redirect(location, status=302)            # HTTP 301/302/307/308
status_only(code)                         # HTTP status with empty body
grpc_response(message, trailers={})       # gRPC unary
grpc_error(code, message)                 # gRPC canonical error
mongodb_document(doc)                     # MongoDB insert/findAndModify result
mongodb_error(message, code=1)            # MongoDB BSON error (ok=0)
cassandra_rows(rows)                      # Cassandra SELECT result
cassandra_error(code, message)            # CQL ERROR frame
clickhouse_rows(rows)                     # ClickHouse JSON format result
clickhouse_ok()                           # ClickHouse successful exec
clickhouse_error(code, message)           # HTTP 500 + DB::Exception body
```

### Protocol 1: HTTP

```python
jwks_keys = {"keys": [{"kty": "OKP", "crv": "Ed25519", "kid": "test-1", "x": "..."}]}

auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration/jwks": json_response(200, jwks_keys),
        "GET /.well-known/openid-configuration":     json_response(200, {"issuer": "http://auth:8090"}),
        "POST /token":                               json_response(200, {"access_token": "test", "expires_in": 3600}),
        "GET /health":                               status_only(204),
    },
)
```

- **Route key format:** `METHOD PATTERN`. Pattern supports `*` (single
  segment) and `**` (multi-segment) globs.
- **Match order:** longest-prefix, most-specific first. Ties resolved by
  insertion order in the dict.
- **Unmatched requests:** return 404 by default (configurable via
  `default = status_only(501)`).

### Protocol 2: TCP

Raw TCP request/response matching for binary protocols:

```python
legacy = mock_service("legacy-tcp",
    interface("main", "tcp", 9000),
    routes = {
        # Match prefix, return fixed bytes
        b"PING\n":         bytes_response(0, b"PONG\n"),
        b"VERSION\n":      bytes_response(0, b"2.0.0\n"),
    },
    default = bytes_response(0, b"ERR unknown\n"),
)
```

- Keys are `bytes` literals matched as **line prefixes** (newline-framed)
  or **length-prefixed frames** (when framing is specified).
- For unframed streams, a `matcher=` callable can be supplied.

### Protocol 3: UDP

Datagram-oriented stub:

```python
statsd = mock_service("statsd",
    interface("main", "udp", 8125),
    # UDP mocks default to swallow-and-record: no response, but every
    # datagram emitted as an event for assertions.
)

assert_eventually(events()
    .where(service = "statsd", op = "recv")
    .count() >= 10)
```

Optional `routes={}` provides response datagrams keyed by prefix.

### Protocol 4: gRPC

```python
flags = mock_service("flags",
    interface("main", "grpc", 50051),
    proto = "./flags.proto",  # optional — enables typed messages
    routes = {
        "/flags.v1.Flags/Get":  grpc_response({"enabled": True, "variant": "B"}),
        "/flags.v1.Flags/List": grpc_response({"flags": [{"name": "rollout", "enabled": True}]}),
        "/flags.v1.Flags/Fail": grpc_error("UNAVAILABLE", "backend down"),
    },
)
```

- Without `proto=`: accept any wire format, reply with JSON payload
  encoded as `google.protobuf.Struct`. Works for clients that decode loosely
  (most Go / Node clients via reflection).
- With `proto=`: payloads are validated against the FileDescriptor and
  encoded in binary form.
- gRPC reflection enabled by default so `grpcurl` works against the stub.

### Protocol 5: Redis (via `@faultbox/mocks/redis.star`)

```python
load("@faultbox/mocks/redis.star", "redis")

cache = redis.server(
    name      = "redis-stub",
    interface = interface("main", "redis", 6379),
    state = {
        "config:max_retries": "3",
        "config:timeout_ms":  "5000",
        "flag:new_ui":        "true",
    },
    # Optional: override specific commands
    overrides = {
        "INFO":      text_response(0, "redis_version:7.0.0\r\n"),
        "CLUSTER *": text_response(0, "cluster_enabled:0\r\n"),
    },
)
```

- `state=` seeds an in-memory key/value map; GET/SET/DEL/EXISTS/INCR/TTL
  operate on it naturally. No persistence — wiped at runtime shutdown.
- `overrides=` replaces specific commands (pattern match on verb + args).
- Covers the 80% of Redis usage that is string cache + counters. Streams,
  pub/sub, Lua scripting are out of scope for v1.

### Protocol 6: Kafka (via `@faultbox/mocks/kafka.star`)

```python
load("@faultbox/mocks/kafka.star", "kafka")

events_bus = kafka.broker(
    name      = "kafka-stub",
    interface = interface("main", "kafka", 9092),
    topics = {
        "orders":   [{"id": 1, "item": "apple"}, {"id": 2, "item": "pear"}],
        "payments": [],  # empty topic, accept produces
    },
)

# Consumer reads pre-seeded messages
# Producer writes land in topic, visible via events() and assert_eventually()
```

- Mock broker answers `ApiVersions`, `Metadata`, `Produce`, `Fetch`,
  `FindCoordinator`, `OffsetFetch`, `OffsetCommit`, `JoinGroup`,
  `Heartbeat` — the subset real clients need for simple produce/consume.
- Topic creation is implicit (no `CreateTopics` required).
- Produced messages are both stored and emitted as events for assertions.

### Protocol 7: MongoDB (via `@faultbox/mocks/mongodb.star`)

```python
load("@faultbox/mocks/mongodb.star", "mongo")

users_db = mongo.server(
    name      = "users-stub",
    interface = interface("main", "mongodb", 27017),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob", "role": "user"},
        ],
    },
    overrides = {
        "users.insert": mongodb_error("disk full"),
    },
)
```

- `collections=` seeds documents per collection; `find` / `findOne` /
  `count` answer from the in-memory store.
- `overrides=` replaces specific `collection.op` pairs with canned
  responses — identical semantics to the existing proxy recipe matcher.
- Wire format: real BSON OP_MSG encoding (shared with RFC-016 MongoDB
  proxy), so real drivers decode responses correctly.

### Dynamic handlers

Routes accept a `dynamic(fn)` wrapper for per-request logic. The callable
receives a request dict and returns a response value:

```python
def sign_jwt(request):
    claims = {
        "sub":   request["query"].get("user", "anonymous"),
        "exp":   now() + 3600,
        "iat":   now(),
        "iss":   "http://auth:8090",
    }
    return json_response(200, {"access_token": jwt.sign_ed25519(claims, ED25519_KEY)})

auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration/jwks": json_response(200, jwks_keys),
        "POST /token": dynamic(sign_jwt),
    },
)
```

- Dynamic handlers cover the 20% of cases static responses miss —
  primarily JWT signing with dynamic claims, feature-flag lookups that
  depend on request headers, metadata endpoints that echo query params.
- Starlark-on-hot-path cost is acceptable for mock services: they target
  low RPS (<1000 QPS) by design. Benchmarks belong in real services.
- Available on HTTP, HTTP/2, TCP, UDP, gRPC routes in v1. Protocol
  stdlib mocks (Kafka, Redis, MongoDB) can compose dynamic handlers via
  `overrides=` in follow-up work.

### TLS

Real OIDC issuers serve HTTPS. Mock services accept a `tls=True` kwarg:

```python
auth = mock_service("auth",
    interface("https", "http", 8443),
    tls    = True,
    routes = {...},
)
```

- Runtime generates a self-signed cert per mock at startup, signed by a
  per-run Faultbox CA.
- In container mode, the CA bundle is mounted into each SUT container at
  `/etc/ssl/certs/faultbox-ca.pem` and the container's `SSL_CERT_FILE`
  env var points at it.
- Avoids the `SSL_VERIFY=false` env-var smell that ships to production
  otherwise.

### Protocol plugin contract

Each protocol plugin opts into mocking by implementing the optional
`MockHandler` interface alongside `Protocol`:

```go
// MockHandler is an optional capability. Protocol plugins that support
// mock_service() implement it; others do not and mock_service() rejects
// their interface type at spec load time.
type MockHandler interface {
    // Serve starts an in-process listener on addr and serves the given
    // mock spec until ctx is cancelled. Each handled request MUST emit
    // an event via the supplied Emitter so assertions see it.
    Serve(ctx context.Context, addr string, spec MockSpec, emit Emitter) error
}

type MockSpec struct {
    Routes  map[string]MockResponse
    State   map[string]string
    Topics  map[string][]any
    Default *MockResponse
}
```

Runtime behavior:

1. On spec load, `mock_service()` asserts each declared interface's
   protocol registers a `MockHandler`. If not, load fails with a clear
   error: `protocol "postgres" does not support mock_service() yet`.
2. At test start, runtime spawns a goroutine per interface running
   `Serve()`. Ports are bound on 127.0.0.1 (or docker network IP in
   container mode).
3. Runtime never installs seccomp filters on mock services — there is no
   process to filter.
4. `fault()` still works: mock traffic flows through the same proxy layer,
   so `fault(auth.http, response(path="/token", status=500))` overrides
   the stub response exactly as if it were a real service.

### Event emission

Every handled request emits one event with this shape:

```
{
    "service": "auth",
    "interface": "http",
    "op": "GET /.well-known/openid-configuration/jwks",
    "request":  { "headers": {...}, "body": "..." },
    "response": { "status": 200, "body_size": 412 },
    "duration_ms": 0,
    "kind": "mock",
}
```

This makes mock services **fully observable** via the existing `events()`
DSL. Users can assert on call counts, call order, request contents, etc.

### Integration with `fault()`

Mock services accept the same fault rules as real services:

```python
auth = mock_service("auth", interface("main", "http", 8090), routes = {...})

fault_scenario("jwks_unavailable",
    target = auth.main,
    rules  = [error(path = "/.well-known/**", status = 503)],
)
```

The mock responds normally, then the proxy layer rewrites the response —
same machinery RFC-001 introduced for real services. Zero new fault code.

### Composition with `service()`

Mock services sit next to real services in the topology:

```python
auth  = mock_service("auth",  interface("http", "http", 8090), routes = {...})
redis = mock_service("redis", interface("main", "redis", 6379), state = {...})

api   = service("api",
    interface("http", "http", 8080),
    image = "myapp:latest",
    depends_on = [auth, redis],
    env = {
        "JWKS_URL":  "http://auth:8090/.well-known/openid-configuration/jwks",
        "REDIS_URL": "redis://redis:6379",
    },
)
```

In container mode, mocks bind inside the Faultbox docker network so the
real service under test reaches them by hostname.

### Reuse from RFC-016

The MockHandler implementation for each protocol can reuse the real-service
infrastructure shipped in RFC-016:

- **MongoDB**: parseOPMSG + real BSON encoding (done in RFC-016)
- **HTTP/2**: h2c server + httputil.ReverseProxy handler pattern (done)
- **Cassandra**: CQL frame reader/writer + error frame emission (done)
- **ClickHouse**: SQL body extraction + HTTP 500 error shape (done)
- **UDP**: datagram relay (done, extend to match+respond vs match+swallow)

For these five, mock service support is mostly a matter of implementing
`MockHandler.Serve()` that generates responses rather than forwarding.
The wire format work is already in-tree.

## Open questions (resolved)

1. **Dynamic handlers** — ✅ **Resolved:** ship in v1 for HTTP, HTTP/2,
   TCP, UDP, gRPC via the `dynamic(fn)` wrapper. JWT signing is the
   primary driver — static responses don't cover real OIDC stubs.
   Starlark hot-path cost is acceptable for mock services (<1000 QPS
   design target). Protocol stdlib mocks (Kafka/Redis/MongoDB) compose
   `dynamic()` via their `overrides=` kwarg.

2. **TLS** — ✅ **Resolved:** ship in v1. Self-signed cert per mock,
   signed by a per-run Faultbox CA. CA bundle mounted into SUT
   containers at `/etc/ssl/certs/faultbox-ca.pem` with `SSL_CERT_FILE`
   env var pointing at it. Pulled forward because OIDC issuers are
   `https://` and the alternative (disabling cert validation in SUT env)
   is a worse outcome.

3. **Persistence across reuse** — ✅ **Resolved:** stateless by default.
   Matches the "stub" mental model. Persistent state is a different
   product (real service). No `persist=True` kwarg — if you need it,
   run the real service.

4. **Postgres/MySQL (SQL mocks)** — ⏭️ **Spun off to
   [RFC-020 (#39)](https://github.com/faultbox/Faultbox/issues/39).**
   SQL mock semantics are their own design problem (query matching,
   typed result set encoding, prepared statements, transaction state).
   Not in v0.8.0 scope.

5. **OpenAPI / proto import** — ⏭️ **Spun off to
   [RFC-021 (#40)](https://github.com/faultbox/Faultbox/issues/40).**
   Contract-driven mock generation is a separate design (schema parsing,
   example selection, validation modes). Not in v0.8.0 scope.

## Phasing

Updated 2026-04-19 after scope review. v0.8.0 ships the full request/response
mock surface plus the three highest-demand stateful protocols as stdlib
mocks. Each phase below is independently shippable.

| Phase | Protocols / Capabilities | Target release | Rationale |
|---|---|---|---|
| **v1** | HTTP, HTTP/2, TCP, UDP generic `mock_service()` + `dynamic()` handlers + TLS | **v0.8.0** | Auth stubs (JWKS/OIDC), metrics sinks, legacy TCP services, service-mesh traffic. JWT signing and HTTPS both required for the motivating use case. |
| **v1 stretch** | gRPC `mock_service()` | **v0.8.0** if schedule permits; else **v0.9.0** | Feature-flag stubs, license validators. Same route-map shape as HTTP; `proto=` optional. |
| **v1 stdlib** | `@faultbox/mocks/kafka.star`, `@faultbox/mocks/redis.star`, `@faultbox/mocks/mongodb.star` | **v0.8.0** | Highest-demand stateful stubs; reuse RFC-016 wire-format infrastructure (BSON, RESP, Kafka protocol) already in-tree. |
| **v2** | `@faultbox/mocks/cassandra.star`, `@faultbox/mocks/clickhouse.star`, `@faultbox/mocks/nats.star` | **v0.9.0** | Lower-demand stateful stubs. CQL + NATS subject routing both require more wire work. |
| **v3** | SQL mocks (Postgres, MySQL) | See [RFC-020 (#39)](https://github.com/faultbox/Faultbox/issues/39) | Separate RFC. |
| **v4** | OpenAPI / proto generation | See [RFC-021 (#40)](https://github.com/faultbox/Faultbox/issues/40) | Separate RFC. |

### Per-protocol mock surface (updated)

| Protocol | Entry point | Shape | Notes |
|---|---|---|---|
| `http` / `http2` | `mock_service()` generic | `routes = {"METHOD /path": response}` | Same handler; HTTP/2 adds h2c |
| `tcp` | `mock_service()` generic | `routes = {b"prefix\n": bytes_response(...)}` | Line-framed or `matcher=` callable |
| `udp` | `mock_service()` generic | `routes = {b"prefix": bytes_response(...)}` | Swallow-and-record by default |
| `grpc` | `mock_service()` generic | `routes = {"/pkg.Service/Method": response}` | Reflection on; `proto=` optional |
| `redis` | `redis.server()` stdlib | `state = {...}`, `overrides = {...}` | Covers string cache + counters |
| `kafka` | `kafka.broker()` stdlib | `topics = {...}` | Answers ApiVersions, Produce, Fetch |
| `mongodb` | `mongo.server()` stdlib | `collections = {...}`, `overrides = {...}` | Real BSON OP_MSG (shared with RFC-016 proxy) |
| `cassandra` | `cassandra.server()` stdlib (v2) | `rows = {...}`, `overrides = {...}` | Real CQL v4 frames |
| `clickhouse` | `clickhouse.server()` stdlib (v2) | `rows = {...}`, `overrides = {...}` | HTTP interface + SQL body match |

### Examples for stdlib protocols

**MongoDB mock** (v0.8.0):

```python
load("@faultbox/mocks/mongodb.star", "mongo")

users_db = mongo.server(
    name        = "users-stub",
    interface   = interface("main", "mongodb", 27017),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob", "role": "user"},
        ],
    },
    overrides = {
        "users.insert": mongodb_error("disk full"),
    },
)
```

**HTTP/2 mock** (v0.8.0 — identical route format to HTTP):

```python
gw = mock_service("gw",
    interface("public", "http2", 8080),
    routes = {
        "GET /healthz":    status_only(200),
        "POST /api/v1/**": json_response(200, {"ok": True}),
    },
)
```

**ClickHouse mock** (v0.9.0):

```python
load("@faultbox/mocks/clickhouse.star", "clickhouse")

analytics = clickhouse.server(
    name      = "analytics-stub",
    interface = interface("main", "clickhouse", 8123),
    rows = {
        "SELECT count() FROM events*": [{"count": 42}],
    },
    overrides = {
        "INSERT*": clickhouse_ok(),
    },
)
```

## Non-goals

- **Not a general-purpose mock framework.** Faultbox mocks exist to stand
  up dependencies of the system under test. Not trying to replace WireMock,
  MockServer, or Prism.
- **Not an API simulator.** No stateful behavior modeling (e.g. CRUD
  semantics, transactions). For that, run the real service.
- **Not a load generator.** Mocks are optimized for correctness, not
  throughput. Benchmarks belong in real services.

## Alternatives considered

1. **Ship a `faultbox/mock` base image with Python+handlers.** Rejected:
   still a container, still needs sync with the spec, still imports
   Python into the dep graph.
2. **Teach `response()` to synthesize responses without an upstream.**
   Rejected: conflates fault injection with service provisioning. Two
   concepts, two primitives.
3. **Generate mock binaries from the spec at `faultbox test` time.**
   Rejected: adds a compile step, needs a Go toolchain in every dev
   environment.

## Success criteria

- The JWKS customer use case works in ≤10 lines of spec, no extra files.
- Mock services are indistinguishable from real services in:
  - Event log output
  - `fault()` rule application
  - `events()` / `assert_eventually()` assertions
- No new spec concepts users must learn beyond `mock_service()` and the
  response constructors.
- First-run (cold) mock service startup: <50ms per interface.
- Zero container/image/network cost when a spec uses only mocks.

## Appendix: full customer example

```python
# The example from the RFC prompt, working end-to-end in v1.

jwks = {
    "keys": [{
        "kty": "OKP", "crv": "Ed25519", "kid": "test-1",
        "x": "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo",
    }],
}

auth = mock_service("auth-stub",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration/jwks": json_response(200, jwks),
        "GET /.well-known/openid-configuration":      json_response(200, {
            "issuer": "http://auth-stub:8090",
            "jwks_uri": "http://auth-stub:8090/.well-known/openid-configuration/jwks",
        }),
    },
)

api = service("api",
    interface("main", "http", 8080),
    build = "./api",
    depends_on = [auth],
    env = {"OIDC_ISSUER": "http://auth-stub:8090"},
)

def test_authenticated_request():
    resp = api.main.get(path = "/me", headers = {"Authorization": "Bearer ..."})
    assert_eq(resp.status, 200)

    # Verify the app actually fetched JWKS (not just cached from a previous run)
    assert_eventually(events()
        .where(service = "auth-stub", op = "GET /.well-known/openid-configuration/jwks")
        .count() >= 1)
```
