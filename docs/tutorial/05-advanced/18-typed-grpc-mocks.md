# Chapter 18: Typed gRPC Mocks — Stub Production gRPC Services in Starlark

**Duration:** 20 minutes
**Prerequisites:** [Chapter 17 (Mock Services)](17-mock-services.md), a working `protoc` and some familiarity with `.proto` files

## Goals & Purpose

Your service talks to a handful of gRPC dependencies — geo-config,
user-service, balance-api — all via compiled `*.pb.go` stubs. You want
to run it under Faultbox without those real services running.

Chapter 17's generic `mock_service()` gets you 90% of the way, but the
last 10% bites: the native mock encodes responses as
`google.protobuf.Struct` because it has no schema for your services.
Typed Go clients reject `Struct` payloads at decode time —
`pkg.NewClient(conn).GetCity(ctx, req)` either fails or silently
returns zero values.

**Typed gRPC mocks** (shipped in v0.9.0) close that gap. You hand
Faultbox a `FileDescriptorSet` — a compact binary that `protoc` emits
from your `.proto` files — and the mock encodes responses as real
message types on the wire. Your compiled-stub client decodes them
normally, as if it were talking to the real upstream.

This chapter teaches you to:

- **Build a `FileDescriptorSet` (`.pb` file)** from your existing
  `.proto` files using `protoc`.
- **Load it into a `grpc.server()`** via the `descriptors=` kwarg.
- **Declare typed responses** as Starlark dicts that Faultbox encodes
  against the right message type at request time.
- **Debug with grpcurl** — typed mocks auto-register the standard
  gRPC reflection service.
- **Compose typed mocks with real services and fault rules** in the
  same topology.

After this chapter, you can swap a real gRPC upstream for a typed mock
with one `.pb` file and a page of Starlark — no Go binary, no
Dockerfile.

## Why descriptor sets, not `.proto` files

Faultbox deliberately does not ship `protoc` and does not parse
`.proto` files directly. Reason: customers already have proto build
pipelines. A monorepo pattern — proto source and pre-built `.pb.go`
in a dedicated repository — is the norm. Faultbox plugs into that
pipeline by consuming the output, not by trying to own the build.

Generate the `.pb` file once:

```bash
protoc \
    --include_imports \
    --descriptor_set_out=./proto/upstreams.pb \
    proto/yourcorp/geo_config/*.proto \
    proto/yourcorp/user/*.proto
```

- `--descriptor_set_out=<path>` — output the compiled descriptors as
  a single `.pb` file (binary-encoded `google.protobuf.FileDescriptorSet`).
- `--include_imports` — include transitive `.proto` dependencies so
  the set is self-contained.

The standard `google.protobuf.*` well-known types (`Timestamp`,
`Empty`, `Any`, `Struct`, `Duration`, `FieldMask`, wrappers) are
pre-registered by Faultbox — you don't need to include them in your
`.pb`, and your customer protos that import them resolve automatically.

## A first typed mock

Suppose your SUT calls `/yourcorp.geo.GeoService/GetCity` and expects
a typed `yourcorp.geo.City` back.

```python
# faultbox.star
load("@faultbox/mocks/grpc.star", "grpc")

geo = grpc.server(
    name        = "geo",
    interface   = interface("main", "grpc", 9001),
    descriptors = "./proto/upstreams.pb",
    services    = {
        "/yourcorp.geo.GeoService/GetCity": {
            "response": {
                "id":       42,
                "name":     "Almaty",
                "country":  "KZ",
                "currency": "KZT",
            },
        },
    },
)

api = service("api",
    binary = "./bin/api",
    interface("public", "http", 8080),
    env = {
        "GEO_GRPC_ADDR": geo.main.internal_addr,
    },
    depends_on = [geo],
    healthcheck = http("localhost:8080/health"),
)

def test_city_lookup():
    r = http.get(api.public, "/cities/42")
    assert_eq(r.status, 200)
    assert_eq(r.json.name, "Almaty")
```

Run it:

```bash
$ faultbox test ./faultbox.star --test city_lookup
```

Under the hood: when the api calls `GetCity`, Faultbox looks up the
method's output type in your `.pb` (`yourcorp.geo.City`), encodes
the Starlark dict as a typed `City` on the wire, and the api's
compiled `*.pb.go` stub decodes it normally. Zero difference from a
real upstream on the decode side.

## Response shapes

Each entry in the `services={...}` dict is one of four shapes:

### `{"response": <dict>}` — happy-path typed response

The dict is encoded against the method's output descriptor at request
time. Unknown fields surface as errors (`unknown field "cityid"
(did you mean "city_id"?)`) — you'll see them the first time the SUT
hits the route.

```python
"/yourcorp.geo.GeoService/GetCity": {
    "response": {"id": 42, "name": "Almaty"},
},
```

### `{"error": {"code": "X", "message": "..."}}` — status-code error

The mock returns a gRPC error with the specified status code.
`code` is either a canonical name (`"UNAVAILABLE"`,
`"PERMISSION_DENIED"`, etc.) or an integer (1–16).

```python
"/yourcorp.geo.GeoService/AdminUpdate": {
    "error": {"code": "PERMISSION_DENIED", "message": "admin only"},
},
```

### `grpc.dynamic(fn)` — per-request Starlark handler

When canned responses aren't enough, a Starlark function receives the
request and returns a response:

```python
def handle_by_coords(req):
    # req.body is the decoded request dict; for typed mocks, not yet
    # available in v0.9.0 (JSON-only). Use routes + response for most
    # cases; dynamic for pure-response logic.
    return grpc.response({
        "id":   1 if req.body else 2,
        "name": "dynamic",
    })

"/yourcorp.geo.GeoService/GetCityByCoords": grpc.dynamic(handle_by_coords),
```

### `grpc.raw_response(bytes)` — pre-encoded wire bytes escape hatch

For cases the typed encoder can't express — oneof tricks, deprecated
fields, extensions — pass the exact wire bytes:

```python
"/yourcorp.geo.GeoService/Exotic": grpc.raw_response(b"\x08\x2a"),
```

## Multi-service mock processes

When several gRPC services share a single mock process on different
ports — the truck-api pattern — declare one `grpc.server()` per service,
all sharing the same `.pb` file:

```python
descriptors = "./proto/upstreams.pb"

geo = grpc.server(
    name        = "geo",
    interface   = interface("main", "grpc", 9001),
    descriptors = descriptors,
    services    = { "/yourcorp.geo.GeoService/GetCity": {...} },
)
user = grpc.server(
    name        = "user",
    interface   = interface("main", "grpc", 9003),
    descriptors = descriptors,
    services    = { "/yourcorp.user.UserService/GetUser": {...} },
)

api = service("api",
    binary = "./bin/api",
    interface("public", "http", 8080),
    env = {
        "GEO_GRPC_ADDR":  geo.main.internal_addr,
        "USER_GRPC_ADDR": user.main.internal_addr,
    },
    depends_on = [geo, user],
)
```

Each mock is an independent Faultbox service, so fault rules target
them one at a time without leaking.

## Faulting a typed mock

`@faultbox/recipes/grpc.star` fault recipes work unchanged — the
fault layer sits in front of the typed encoder:

```python
load("@faultbox/recipes/grpc.star", "grpc_faults")

geo_down = fault_assumption("geo_down",
    target = geo.main,
    rules  = [grpc_faults.unavailable()],
)

test_retries_on_geo_down = fault_scenario("retries_on_geo_down",
    scenario = create_order,
    faults   = [geo_down],
    expect   = lambda r: assert_eq(r.status, 200),  # retry succeeds
)
```

The fault fires at the proxy layer; the typed encoder never runs on
the faulted request.

## grpcurl support

Typed mocks automatically register the standard gRPC reflection v1
service. Point `grpcurl` at the mock and it discovers everything:

```bash
$ grpcurl -plaintext localhost:9001 list
grpc.reflection.v1.ServerReflection
yourcorp.geo.GeoService

$ grpcurl -plaintext localhost:9001 describe yourcorp.geo.GeoService
yourcorp.geo.GeoService is a service:
service GeoService {
  rpc GetCity(GetCityRequest) returns (City);
}

$ grpcurl -plaintext -d '{"id":1}' localhost:9001 yourcorp.geo.GeoService/GetCity
{
  "id": "42",
  "name": "Almaty",
  "country": "KZ",
  "currency": "KZT"
}
```

Useful for debugging when your mock's behavior doesn't match what the
SUT expects — you can exercise the mock directly without running the
SUT.

Reflection is only registered when `descriptors=` is set; untyped
`mock_service()` gRPC mocks don't auto-register to avoid surprises.

## What's not in v1

[RFC-023](../../rfcs/0023-typed-proto-grpc-mocks.md) scoped v0.9.0 to
the 90% case. Deferred:

- **Streaming RPCs** — unary only. Server-streaming, client-streaming,
  and bidi are separate design problems; revisit when a real customer
  needs them.
- **Custom error details** (`google.rpc.Status` with typed `details`).
  Plain status-code errors only.
- **Load-time response-shape validation.** A typo like `"cityid"`
  instead of `"city_id"` surfaces at request time with a clear error
  message. Load-time is a v2 ergonomic win.
- **Raw `.proto` ingestion.** You generate the `.pb` via `protoc`;
  Faultbox consumes it. Parsing `.proto` directly would re-import the
  complexity we delegated to your build system.
- **Connect gRPC-Web and pure Connect protocol.** Standard gRPC and
  `connect-go` with `connect.WithGRPC()` both work; other Connect
  flavors need a separate handler.

## When to use this vs. `mock_service()`

- **Your SUT uses compiled Go/C++/Rust gRPC stubs** → typed gRPC mock.
  The generic `Struct` payload won't decode.
- **Your SUT uses reflection-based clients** (Node, some Python
  setups) → either works. `mock_service()` is simpler because it
  skips the `.pb` step.
- **You're testing a feature flag fetch, OIDC JWKS, or other
  not-really-gRPC-but-HTTP endpoint** → use Chapter 17's
  `mock_service()` for HTTP. Don't reach for gRPC if you don't need it.

## When to drop to a Go binary

Typed Starlark mocks don't cover every case. If you need:

- Streaming RPCs (server/client/bidi).
- Complex server-side logic that `grpc.dynamic()` can't express
  (stateful accumulation across calls, deep field inspection of the
  request, etc.).
- Custom TLS client-cert handling.
- A protocol flavor Faultbox doesn't speak natively.

… drop to a Go binary that imports your real `*.pb.go`. See the
`service(binary=...)` pattern in
[mock-services.md](../../mock-services.md#go-binary-fallback-for-exotic-cases).
For everything else in v0.9.0, start with `grpc.server()`.

## Summary

- **`grpc.server(descriptors="./x.pb", services={...})`** is the
  primary way to mock gRPC upstreams with typed responses.
- **Generate `.pb` via `protoc --descriptor_set_out=...`** — Faultbox
  doesn't wrap protoc; your existing build owns that.
- **Four response shapes:** `{"response": dict}`, `{"error": {...}}`,
  `grpc.dynamic(fn)`, `grpc.raw_response(bytes)`.
- **Reflection auto-registers** — grpcurl works out of the box.
- **Fault recipes compose unchanged** — fault the typed mock as if
  it were a real upstream.
- **RFC-023 scope is the 90% case.** Streaming, rich errors, and
  alternate Connect flavors are explicit v2 candidates.
