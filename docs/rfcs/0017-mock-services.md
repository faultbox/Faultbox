# RFC-017: Native Mock Services — Stub Protocols Without Containers

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-04-17
- **Branch:** `rfc/017-mock-services`
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

### The builtin

```python
mock_service(name, *interfaces,
    routes:  dict = {},      # protocol-specific route/handler map
    state:   dict = {},      # initial key/value state (Redis-like stubs)
    topics:  dict = {},      # pre-seeded Kafka topics
    depends_on: list = [],   # same semantics as service()
    # Fault/reuse/seed/reset are NOT supported — mocks are always reusable
    # and stateless-by-default; use `state=` for seed data.
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

### Protocol 5: Redis

```python
cache = mock_service("redis-stub",
    interface("main", "redis", 6379),
    state = {
        "config:max_retries": "3",
        "config:timeout_ms":  "5000",
        "flag:new_ui":        "true",
    },
    # Optional: override specific commands
    routes = {
        "INFO":      text_response(0, "redis_version:7.0.0\r\n"),
        "CLUSTER *": text_response(0, "cluster_enabled:0\r\n"),
    },
)
```

- `state=` seeds an in-memory key/value map; GET/SET/DEL/EXISTS/INCR/TTL
  operate on it naturally. No persistence — wiped at runtime shutdown.
- `routes=` overrides specific commands (pattern match on verb + args).
- Covers the 80% of Redis usage that is string cache + counters. Streams,
  pub/sub, Lua scripting are out of scope for v1.

### Protocol 6: Kafka

```python
events_bus = mock_service("kafka-stub",
    interface("main", "kafka", 9092),
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

## Open questions

1. **Dynamic handlers.** Should routes accept Starlark callables
   `(request) -> response` for per-request logic (e.g. JWT signing with
   dynamic claims)? Starlark on the hot path is expensive. Proposal:
   static-only in v1, add `dynamic(fn)` wrapper in a follow-up RFC.

2. **TLS.** Auth providers typically serve HTTPS. Proposal: generate a
   self-signed cert per mock at startup, expose via `tls=True` kwarg.
   Clients under test must trust the Faultbox CA (mounted into the
   container via a known volume).

3. **Persistence across reuse.** Mock services implicitly behave like
   `reuse=True`. Should `state=` be reset between tests by default?
   Proposal: yes — matches the "stateless stub" mental model. Opt into
   persistence with `persist=True`.

4. **Postgres/MySQL.** SQL mocks require parsing SQL or pattern-matching
   queries. Complex enough to warrant its own RFC. Explicitly out of
   scope here.

5. **OpenAPI / proto import.** Long-term: `mock_service("api", openapi =
   "./spec.yaml")` auto-generates routes from the OpenAPI document.
   Deferred to a follow-up.

## Phasing

Updated after RFC-016 landed (MongoDB, HTTP/2, UDP, Cassandra,
ClickHouse as real-service protocols). Each phase below is independently
shippable.

| Phase | Protocols | Rationale |
|---|---|---|
| **v1** | HTTP, HTTP/2, TCP, UDP | Auth stubs, metrics sinks, legacy TCP services, service-mesh traffic. HTTP and HTTP/2 share a handler (HTTP/2 adds h2c; same route map). |
| **v2** | gRPC, Redis, Kafka, ClickHouse | Needs protocol-aware encoding. ClickHouse mocks are cheap (HTTP interface + SQL body match); included in v2 alongside the other body-parsing mocks. |
| **v3** | MongoDB, Cassandra, NATS | BSON / CQL encoding. MongoDB is the highest-demand of the v3 set (Node.js/Python ecosystems); Cassandra mocks need CQL frame emission with realistic error codes. |
| **v4** | Postgres, MySQL | SQL parsing is its own design problem — a mock DB would need to answer `SELECT` with canned rows shaped like real result sets. Deferred until there's clear demand. |

### Per-protocol mock surface (updated)

| Protocol | `routes=` keyed by | State helpers | Notes |
|---|---|---|---|
| `http` / `http2` | `"METHOD /path"` | — | Same handler; HTTP/2 adds h2c |
| `tcp` | `bytes` prefix | — | Line-framed or `matcher=` callable |
| `udp` | `bytes` prefix | — | Swallow-and-record by default |
| `grpc` | `"/pkg.Service/Method"` | — | Reflection on; `proto=` optional |
| `redis` | `"CMD arg"` glob | `state={}` KV seed | Covers string cache + counters |
| `kafka` | — (use `topics=`) | `topics={}` pre-seeded | Answers ApiVersions, Produce, Fetch |
| `mongodb` | `"collection.op"` | `collections={}` seed | BSON-encoded responses; use real mongo driver to decode |
| `cassandra` | `"CQL pattern"` | — | Emits real CQL v4 frames; error code 0x0000 by default |
| `clickhouse` | SQL body pattern | — | HTTP interface; same matcher as RFC-016 proxy uses |

### Examples for new protocols

**MongoDB mock** (v3):

```python
users_db = mock_service("users-stub",
    interface("main", "mongodb", 27017),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob", "role": "user"},
        ],
    },
    routes = {
        "users.insert": mongodb_error("disk full"),  # auth stub rejects writes
    },
)
```

**HTTP/2 mock** (v1 — identical route format to HTTP):

```python
grpc_gateway = mock_service("gw",
    interface("public", "http2", 8080),
    routes = {
        "GET /healthz":    status_only(200),
        "POST /api/v1/**": json_response(200, {"ok": true}),
    },
)
```

**ClickHouse mock** (v2):

```python
analytics = mock_service("analytics-stub",
    interface("main", "clickhouse", 8123),
    routes = {
        # Match SQL body prefix, return fixed JSON result
        "SELECT count() FROM events*": clickhouse_rows([{"count": 42}]),
        "INSERT*":                      clickhouse_ok(),
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
