# Mock Services

Stand up protocol stubs entirely in Starlark — no Dockerfile, no
sidecar, no `python -m http.server` dance. Faultbox starts an
in-process listener for each declared interface, answers traffic on
the wire protocol your SUT expects, and emits every handled request
into the event log so assertions see it.

See [RFC-017](rfcs/0017-mock-services.md) for the design rationale and
[RFC-019](rfcs/0019-recipe-distribution.md) for how the protocol stdlib
ships embedded in the binary.

## When to use a mock vs. the real service

| Situation | Use |
|---|---|
| SUT calls an OIDC issuer for JWKS at startup | **Mock** (`mock_service` with HTTP) |
| SUT publishes orders to Kafka, asserts via `events()` | **Mock** (`@faultbox/mocks/kafka.star`) |
| SUT reads feature flags from gRPC `flags.v1.Flags/Get` | **Mock** (`mock_service` with gRPC) |
| SUT does real CRUD against Postgres in a transaction | **Real** (container, RFC-016 plugin) |
| SUT relies on Redis pub/sub or Lua | **Real** (mock covers string cache + counters only) |
| You want to test how SUT handles a Kafka rebalance | **Real Kafka + recipes** (faults, not mocks) |

Mocks are stubs. They answer requests with canned data so the SUT
boots and reaches the code paths you want to test. They are not
production-faithful simulations.

## Two flavours

Mock services split along protocol semantics:

1. **Request/response protocols** — HTTP, HTTP/2, TCP, UDP, gRPC —
   use the generic `mock_service()` builtin with `routes={}`.
2. **Stateful / message-broker protocols** — Kafka, Redis, MongoDB —
   use protocol-specific stdlib constructors loaded from
   `@faultbox/mocks/<name>.star`.

The split is purely at the Starlark surface: the same Go runtime
infrastructure powers both. Stdlib constructors are thin Starlark
wrappers that translate protocol-specific kwargs (`topics=`, `state=`,
`collections=`) into the opaque `config=` map on `mock_service()`.

## The generic primitive

```python
mock_service(name, *interfaces,
    routes:     dict = {},     # protocol-specific pattern → response
    default:    MockResponse = None,
    tls:        bool = False,
    config:     dict = {},     # opaque, used by stdlib wrappers
    depends_on: list = [],
)
```

| Param | What it does |
|---|---|
| `name` | Service name. Reachable as `<name>:<port>` from other services. |
| `*interfaces` | One or more `interface(name, protocol, port)` tuples. |
| `routes` | Pattern → response map. Pattern format depends on protocol (see below). |
| `default` | Fallback response when no route matches. Default: HTTP 404 / protocol-appropriate error. |
| `tls` | When True, terminate TLS using a per-runtime mock CA. See [TLS](#tls) below. |
| `config` | Opaque dict passed to the protocol plugin. Used by stdlib wrappers. |
| `depends_on` | Same semantics as `service()` — start ordering. |

Returns a `ServiceDef` — interchangeable with real services in `fault()`,
`events()`, assertions, env-var references, etc.

## Response constructors

```python
json_response(status=200, body={...}, headers={})
text_response(status=200, body="...", headers={})
bytes_response(status=0, data="raw bytes")
status_only(code)
redirect(location, status=302)
grpc_response(body={...})
grpc_error(code="UNAVAILABLE", message="...")
dynamic(fn)
```

`dynamic(fn)` wraps a Starlark callable that receives a request dict
(`method`, `path`, `headers`, `query`, `body`) and returns a response.
Used for JWT signing, per-request flag lookups, anything where the
canned answer depends on the request.

## HTTP / HTTP/2 mocks

Routes match `"METHOD PATH"` with `*` (single segment) and `**` (any
segments) globs.

```python
auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration":      json_response(200, {
            "issuer":   "http://auth:8090",
            "jwks_uri": "http://auth:8090/.well-known/openid-configuration/jwks",
        }),
        "GET /.well-known/openid-configuration/jwks": json_response(200, {
            "keys": [{"kty": "OKP", "crv": "Ed25519", "kid": "test-1", "x": "..."}],
        }),
        "POST /token":                                dynamic(lambda req: json_response(200, {
            "access_token": sign_jwt({"sub": req["query"].get("user", "anon")}),
            "expires_in":   3600,
        })),
        "GET /health":                                status_only(204),
    },
)
```

HTTP/2 uses the same route table format. Protocol is `"http2"` instead
of `"http"`. Without TLS, served over h2c (cleartext HTTP/2). With
TLS, served over h2 with ALPN.

## TCP mock

Per-connection handler reads one newline-terminated line, matches it
against patterns as **byte prefixes**, writes the configured response,
closes the connection.

```python
legacy = mock_service("legacy",
    interface("main", "tcp", 9000),
    routes = {
        "PING\n":    bytes_response(data = "PONG\n"),
        "VERSION\n": bytes_response(data = "2.0.0\n"),
    },
    default = bytes_response(data = "ERR unknown\n"),
)
```

For non-line-framed protocols (Cassandra CQL frames, custom binary),
use the matching protocol's stdlib mock if available, otherwise run
the real service.

## UDP mock

Default behavior is **swallow-and-record**: every datagram emits a
`mock.recv` event but no response is written. Matches the StatsD /
metrics-sink pattern where the SUT fires datagrams and you assert on
receipt count.

```python
statsd = mock_service("statsd", interface("main", "udp", 8125))

def test_metrics_emitted():
    api.public.post(path = "/order", body = {...})
    assert_eventually(events()
        .where(service = "statsd", op = "recv")
        .count() >= 3)
```

When `routes={}` is set, datagrams whose prefix matches receive the
response written back to their source address.

## gRPC mock

Routes match the full method path `"/pkg.Service/Method"` with
`/pkg.Svc/*` (any method on a service) and `/**` (catch-all) globs.

```python
flags = mock_service("flags",
    interface("main", "grpc", 50051),
    routes = {
        "/flags.v1.Flags/Get":  grpc_response(body = {"enabled": True, "variant": "B"}),
        "/flags.v1.Flags/List": grpc_response(body = {"flags": [{"name": "rollout", "enabled": True}]}),
        "/flags.v1.Flags/Fail": grpc_error(code = "UNAVAILABLE", message = "backend down"),
    },
)
```

Without a `.proto` file, responses are encoded as
`google.protobuf.Struct` — the wire shape that reflection-based and
loose-decoding clients accept. **This works for clients that don't
compile proto stubs** (typed Node clients, Python `grpc` with
generic descriptors, hand-written Go that decodes into
`structpb.Struct`).

**It does not work for clients with compiled proto stubs** — the
standard Go pattern where `pkg.NewServiceClient(conn).Method(ctx,
&pkg.Request{...})` expects a specific generated message type back.
For that case, use the typed gRPC mock below (v0.9.0+).

`grpc_error()` accepts canonical status codes by name (`"OK"`,
`"CANCELLED"`, `"UNKNOWN"`, `"INVALID_ARGUMENT"`, `"DEADLINE_EXCEEDED"`,
`"NOT_FOUND"`, `"ALREADY_EXISTS"`, `"PERMISSION_DENIED"`,
`"RESOURCE_EXHAUSTED"`, `"FAILED_PRECONDITION"`, `"ABORTED"`,
`"OUT_OF_RANGE"`, `"UNIMPLEMENTED"`, `"INTERNAL"`, `"UNAVAILABLE"`,
`"DATA_LOSS"`, `"UNAUTHENTICATED"`) or by numeric value.

## Typed gRPC mocks (`@faultbox/mocks/grpc.star`) — v0.9.0+

For SUTs with compiled proto stubs (the standard Go gRPC pattern),
load your `protoc`-generated `FileDescriptorSet` at spec time and let
Faultbox encode typed responses against it on the wire. One Starlark
file, no Go binary, no Dockerfile — RFC-023.

### Step 1 — Build a `FileDescriptorSet`

Faultbox does not ship `protoc` and does not parse `.proto` files
directly. You generate a `.pb` descriptor file once via your existing
proto build pipeline:

```bash
protoc \
    --include_imports \
    --descriptor_set_out=./proto/all_upstreams.pb \
    proto/inDriver/geo_config/*.proto \
    proto/inDriver/geo_facade/*.proto \
    proto/inDriver/user/*.proto
```

`--include_imports` is important when your protos depend on each other.
The standard `google.protobuf.*` well-known types (`Timestamp`,
`Empty`, `Any`, `Struct`, `Duration`, `FieldMask`, wrappers) are
pre-registered by Faultbox automatically, so you don't need to include
them in the `.pb`.

Monorepo pattern: customers like inDriver maintain proto in a
dedicated repo that publishes both `.proto` source and pre-built
`.pb` artifacts. Point `descriptors=` at the published artifact —
no protoc invocation in your faultbox spec or CI.

### Step 2 — Declare the mock

```python
load("@faultbox/mocks/grpc.star", "grpc")

geo_config = grpc.server(
    name        = "geo-config",
    interface   = interface("main", "grpc", 9001),
    descriptors = "./proto/all_upstreams.pb",
    services    = {
        "/inDriver.geo_config.GeoConfigService/GetCity": {
            "response": {
                "id":       42,
                "name":     "Almaty",
                "country":  "KZ",
                "currency": "KZT",
            },
        },
        "/inDriver.geo_config.GeoConfigService/ListCountries": {
            "response": {
                "countries": [
                    {"code": "KZ", "name": "Kazakhstan"},
                    {"code": "RU", "name": "Russia"},
                ],
            },
        },
        "/inDriver.geo_config.GeoConfigService/AdminUpdate": {
            "error": {"code": "PERMISSION_DENIED", "message": "admin only"},
        },
    },
)
```

Each entry in `services` is one of:

| Form | What it does |
|---|---|
| `{"response": <dict>}` | happy-path response; dict encoded against the method's output type at request time |
| `{"error": {"code": "X", "message": "..."}}` | gRPC status-code error; code is a canonical name or integer |
| `grpc.dynamic(fn)` | per-request Starlark handler; `fn(req)` returns another `grpc.*` response |
| `grpc.raw_response(bytes)` | pre-encoded wire bytes; bypass typed encoder (oneof tricks, extensions) |

### Step 3 — Compose with fault rules

Fault recipes from `@faultbox/recipes/grpc.star` still apply to the
typed mock's interface — same wire-level behavior as the real
upstream:

```python
load("@faultbox/recipes/grpc.star", "grpc_faults")

geo_unstable = fault_assumption("geo_unstable",
    target = geo_config.main,
    rules  = [grpc_faults.unavailable()],
)

deadline_exceeded = fault_scenario("deadline_exceeded",
    scenario = create_order,
    faults   = [geo_unstable],
    expect   = lambda r: assert_eq(r.status, 504),
)
```

### Multi-service mocks (truck-api Phase 1 shape)

When one mock process backs multiple gRPC services on different ports
— the truck-api pattern — declare one `grpc.server()` per service
sharing the same descriptor set:

```python
descriptors = "./proto/all_upstreams.pb"

geo_config = grpc.server(
    name        = "geo-config",
    interface   = interface("main", "grpc", 9001),
    descriptors = descriptors,
    services    = { "/inDriver.geo_config.GeoConfigService/GetCity": {...}, ... },
)
user_service = grpc.server(
    name        = "user-service",
    interface   = interface("main", "grpc", 9003),
    descriptors = descriptors,
    services    = { "/inDriver.user.UserService/GetUser": {...}, ... },
)

api = service("api",
    binary = "./bin/truck-api",
    interface("public", "http", 8080),
    env = {
        "GRPC_GEO_CONFIG_ADDRESS":   geo_config.main.internal_addr,
        "GRPC_USER_SERVICE_ADDRESS": user_service.main.internal_addr,
    },
    depends_on = [geo_config, user_service],
)
```

Each mock is its own service with its own interface, so fault rules
target them independently.

### Reflection support (grpcurl debugging)

When `descriptors=` is set, the mock automatically serves the gRPC
reflection v1 service. `grpcurl` works against the mock out of the
box — no client-side `.proto` file needed:

```bash
$ grpcurl -plaintext localhost:9001 list
grpc.reflection.v1.ServerReflection
inDriver.geo_config.GeoConfigService

$ grpcurl -plaintext localhost:9001 describe inDriver.geo_config.GeoConfigService
inDriver.geo_config.GeoConfigService is a service:
service GeoConfigService {
  rpc GetCity(GetCityRequest) returns (City);
  ...
}

$ grpcurl -plaintext -d '{"id":1}' localhost:9001 inDriver.geo_config.GeoConfigService/GetCity
{
  "id": "42",
  "name": "Almaty",
  "country": "KZ",
  "currency": "KZT"
}
```

### Protocol compatibility

- **gRPC** (standard HTTP/2 + protobuf) — works natively.
- **Connect / connect-go with `WithGRPC()`** — verified; uses the same
  gRPC wire and hits the same handler.
- **Connect gRPC-Web or pure Connect protocol** — not yet supported
  (would need a separate Connect handler alongside the grpc-go server).
  File an issue if you need this.

### What's not in v1 (RFC-023 v2 candidates)

- **Streaming RPCs.** Unary only. Server/client/bidi streaming
  deferred until a customer explicitly needs it.
- **Custom error details** (`google.rpc.Status` with typed `details`).
  Plain status-code errors via `{"error": {"code": "X"}}` only.
- **Load-time response-shape validation.** Response-dict typos surface
  at request time (`unknown field "cityid"` naming the field and the
  target message). Load-time validation is a v2 ergonomic win.
- **Raw `.proto` ingestion.** v1 requires a `FileDescriptorSet` (`.pb`
  file from protoc). Parsing `.proto` directly is v2.

## Go-binary fallback (for exotic cases)

If the typed Starlark mock can't express what you need — streaming
RPCs, dynamic server-side logic that `grpc.dynamic()` can't capture,
or protocols beyond unary gRPC — the Go-binary escape hatch from
v0.8.6 still works:

```python
upstreams = service("upstreams",
    binary = "./bin/upstream-mocks",
    interface("geo_config",   "grpc", 9001),
    interface("user_service", "grpc", 9003),
    healthcheck = tcp("localhost:9001"),
    seccomp    = False,
)
```

The Go binary implements the standard `Unimplemented*Server`
interfaces and imports your real `*.pb.go`. See the v0.8.6 release
notes for a worked example. For new deployments, start with the
typed Starlark mock above; drop to Go only when you hit something it
can't express.

## Kafka stdlib mock

```python
load("@faultbox/mocks/kafka.star", "kafka")

bus = kafka.broker(
    name      = "bus",
    interface = interface("main", "kafka", 9092),
    topics    = {"orders": [], "payments": []},
)
```

Backed by [`github.com/twmb/franz-go/pkg/kfake`](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kfake)
— a battle-tested in-process Kafka broker. Real `kafka-go`,
`franz-go`, `sarama` clients connect, produce, consume, join consumer
groups. Topics are seeded with empty partitions; pre-populating with
messages requires producing them from the spec itself.

| Param | Default | Notes |
|---|---|---|
| `topics` | `{}` | Topic name → list of seed messages (currently messages are ignored; topics created empty). |
| `partitions` | `1` | Default partition count per topic. |

## Redis stdlib mock

```python
load("@faultbox/mocks/redis.star", "redis")

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 6379),
    state = {
        "config:max_retries": "3",
        "config:timeout_ms":  "5000",
        "flag:new_ui":        "true",
    },
)
```

Backed by [`miniredis`](https://github.com/alicebob/miniredis) — full
RESP2 semantics for the 80% of Redis usage that's string cache +
counters + hashes + sorted sets. Real `go-redis`, `redigo`, raw RESP
clients connect and `GET`/`SET`/`DEL`/`INCR`/`EXISTS`/`TTL` operate
on seeded state.

Streams, pub/sub, and Lua scripting are inherited from miniredis but
not all commands behave identically to a real Redis. Specs that depend
on those features should run the real server.

## MongoDB stdlib mock

```python
load("@faultbox/mocks/mongodb.star", "mongo")

users_db = mongo.server(
    name      = "users-stub",
    interface = interface("main", "mongodb", 27017),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob",   "role": "user"},
        ],
    },
)
```

Hand-written BSON OP_MSG + OP_QUERY responder. The official
`mongo-driver/v2` completes its handshake (hello / isMaster /
buildInfo) and `find`/`findOne` returns seeded documents. Writes
(`insert`/`update`/`delete`) acknowledge with `ok: 1` but the data is
discarded — this is a read-through stub. Unknown commands return
`ok: 1` (lenient, so unusual driver chatter doesn't fail tests).

For round-trip CRUD with persistence, run the real server.

## TLS

```python
auth = mock_service("auth",
    interface("https", "http", 8443),
    tls    = True,
    routes = {...},
)
```

When `tls=True`, Faultbox:

1. Generates an ECDSA P-256 mock CA on first use (lazy, one per run).
2. Signs a leaf cert for the mock with SANs `["localhost",
   <service-name>, 127.0.0.1, ::1]`.
3. Writes the CA bundle to `${TMPDIR}/faultbox-ca-<timestamp>.pem`.
4. Wraps the listener with TLS — HTTP, HTTP/2 (ALPN h2), gRPC.

SUTs trust the CA by reading that file. In binary mode, point your
HTTP client's `RootCAs` at it; in container mode, mount it into the
SUT container at a path the client picks up.

TLS is supported on HTTP, HTTP/2, gRPC. Other protocols silently
ignore `tls=True` for now; if you need TLS Kafka/Redis/MongoDB,
run the real service.

## Events

Every mock interaction emits an event. Schema:

```
{
    "service":   "auth",
    "interface": "http",
    "kind":      "mock",
    "type":      "mock.GET /.well-known/openid-configuration/jwks",
    "fields":    { "status": "200", "body_size": "412", ... },
}
```

Use `events().where(service=..., op=...)` to assert call counts, call
order, request contents — same DSL as for real services and syscalls.

## Composing mocks with real services

Mocks coexist with real services in the topology:

```python
load("@faultbox/mocks/redis.star", "redis")

auth  = mock_service("auth",  interface("http", "http", 8090), routes = {...})
cache = redis.server("cache", interface = interface("main", "redis", 6379),
                              state = {"feature:new": "true"})

api = service("api",
    interface("public", "http", 8080),
    image      = "myapp:latest",
    depends_on = [auth, cache],
    env = {
        "JWKS_URL":  "http://auth:8090/.well-known/openid-configuration/jwks",
        "REDIS_URL": "redis://cache:6379",
    },
)
```

The real `api` container reaches `auth` and `cache` by hostname inside
the Faultbox docker network — same as it would reach any other service.

## Faulting a mock

Mocks accept the same `fault()` rules as real services:

```python
auth = mock_service("auth", interface("main", "http", 8090), routes = {...})

jwks_unavailable = fault_assumption("jwks_unavailable",
    target = auth.main,
    rules  = [error(path = "/.well-known/**", status = 503)],
)
```

The mock answers normally, then the proxy layer rewrites the response —
identical to faulting a real service.

## What mocks deliberately don't do

- **Stateful CRUD with persistence.** MongoDB writes are dropped;
  Kafka topic message seeding is empty. Persistence is what real
  services give you.
- **Production-shaped throughput.** Mocks are correctness-first;
  benchmarks belong in real services.
- **Schema validation.** Routes match by pattern, not by request shape.
  Use `dynamic()` if you need to reject malformed requests.
- **Replace WireMock / Prism / mountebank.** Faultbox mocks exist to
  stand up dependencies of the system under test, not to be a
  general-purpose mock platform.
