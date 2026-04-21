# RFC-023: Typed-Proto gRPC Mocks in Starlark

- **Status:** Implemented (v0.9.0, 2026-04-22)
- **Target:** v0.9.0
- **Created:** 2026-04-19
- **Accepted:** 2026-04-21 — open questions resolved
- **Implemented:** 2026-04-22 — 4 phases shipped on `epic/v0.9.0`
- **Discussion:** [#52](https://github.com/faultbox/Faultbox/issues/52)
- **Customer motivation:** truck-api Phase 1 shipped on v0.8.8 using the Go-binary fallback; v0.9.0 collapses that binary to ~10 lines of Starlark.
- **Depends on:** RFC-017 (Native Mock Services — v0.8.0)
- **Go-binary fallback:** retained in `docs/mock-services.md` as a power-user escape hatch for streaming / complex dynamic logic.

## Summary

Faultbox v0.8.0 (RFC-017) shipped native gRPC mocks via the Starlark
`mock_service()` builtin. Today those mocks encode every response as
`google.protobuf.Struct` because the runtime has no proto schema for
the user's services. That works for clients with reflection-based or
loose-decoding stubs (Node, Python, some hand-written Go) — but
**fails for clients with compiled proto stubs**, which is the
overwhelming majority of real-world Go gRPC code.

Customers with codebases like inDriver's truck-api (`github.com/inDriver/geo-config/proto`,
`github.com/inDriver/balance-api/proto`, …) cannot use the native mock
because their app's compiled stubs reject the generic `Struct` payload
at decode time with a type mismatch.

This RFC proposes loading proto descriptors at spec time and using
protoreflect to encode arbitrary message types from Starlark dicts —
closing the gap so customers can write typed gRPC mocks entirely in
their `.star` spec, without dropping back to a hand-written Go binary.

## Motivation

### Current state, in two paragraphs

`mock_service()` for gRPC works today via gRPC's `UnknownServiceHandler`:
the mock accepts any method path (`/pkg.Service/Method`), routes it
through the spec's route table, and returns a payload built by
`grpc_response(body=...)`. Because there's no schema, that payload is
serialized as `google.protobuf.Struct` — a generic JSON-shaped message
type. See `internal/protocol/grpc_mock.go:23-26`.

A client like `pkg.NewServiceClient(conn).Method(ctx, &pkg.Request{...})`
has compiled-in expectations: the response wire bytes must decode as
`pkg.Response`. When the mock sends `google.protobuf.Struct` bytes,
the client's `proto.Unmarshal` either fails outright (proto3 strict
mode) or silently produces a zero-valued response object (proto3
permissive mode). Either way the test is meaningless.

### Why customers need this

From a current customer (truck-api, inDriver):

> Phase 1 plan — mock 8 upstream gRPC services. Recommended approach:
> single Go mock binary that imports the real `github.com/inDriver/*`
> proto packages from go.mod and implements `Unimplemented*Server`
> for each.

The **only** reason they're proposing a Go binary is that the native
Starlark mock can't speak their compiled stubs' message types. Every
other thing they need (port assignments, canned responses, swap-in
fault rules per scenario) is already available in Starlark.

Forcing a Go binary for this means:
- 200+ LOC of boilerplate per customer.
- A separate build step (`go build`) before every `faultbox test` run.
- Coupling the mock to the customer's go.mod — unusable as a generic
  Faultbox feature.
- Defeats RFC-017's "no Dockerfile, no sidecar, just `.star`" pitch.

### What "typed-proto" means here

For each gRPC method `/pkg.Service/Method`, the runtime needs to know:

- The fully qualified request message type and its descriptor.
- The fully qualified response message type and its descriptor.
- The descriptors of any nested / referenced types.
- The descriptors of any imported well-known types
  (`google.protobuf.Timestamp`, `google.protobuf.Empty`, etc.).

Given those, the runtime can:
- Decode incoming request bytes into a `dynamicpb.Message`, exposing
  fields to dynamic Starlark handlers.
- Encode arbitrary Starlark dicts as the correct response type via
  `dynamicpb.NewMessage(descriptor)` + reflection-based field setters.
- Validate at spec-load time that the route map's response shapes are
  type-correct (catch typos before the test runs).

## Technical Details

### Proposed Starlark API

Add a new constructor `grpc.server()` (in `@faultbox/mocks/grpc.star`)
that takes a `descriptors=` kwarg pointing to the proto schema source
and a `services=` map of method-path → handler:

```python
load("@faultbox/mocks/grpc.star", "grpc")

geo_config = grpc.server(
    name = "geo-config",
    interface = interface("main", "grpc", 9001),

    # Where the proto schema comes from (see "Descriptor sources" below).
    descriptors = "./proto/geo_config.pb",      # protoc -o output, OR
    # descriptors = ["./proto/geo_config.proto"],  # raw .proto files
    # descriptors = grpc.descriptors_from_module("github.com/inDriver/geo-config/proto"),

    services = {
        "/inDriver.geo_config.GeoConfigService/GetCity": {
            "response": {
                "city_id":   42,
                "name":      "Almaty",
                "country":   "KZ",
                "currency":  "KZT",
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

        # Status-only error (no body).
        "/inDriver.geo_config.GeoConfigService/AdminUpdate": {
            "error": {"code": "PERMISSION_DENIED", "message": "admin only"},
        },

        # Dynamic handler — receives the typed request as a dict.
        "/inDriver.geo_config.GeoConfigService/GetCityByCoords": grpc.dynamic(
            lambda req: {
                "response": {
                    "city_id": 1 if req["latitude"] > 0 else 2,
                    "name":    "north" if req["latitude"] > 0 else "south",
                },
            },
        ),
    },
)
```

The constructor is a thin wrapper over `mock_service()` that injects
the descriptor set into the gRPC plugin's per-mock state and rewrites
each route's response to be encoded against the resolved response
descriptor.

### Descriptor sources

Three options, in order of friendliness:

1. **`protoc`-generated `FileDescriptorSet` (.pb file).** Single file,
   self-contained, includes well-known type imports already resolved.
   Generated once per customer codebase via a build step:
   ```
   protoc \
     --include_imports --descriptor_set_out=geo_config.pb \
     proto/inDriver/geo_config/*.proto
   ```
   **This is the v1 default.** It separates "Faultbox knows what
   your protos look like" from "Faultbox knows where your protos
   live" — the customer's monorepo conventions stay opaque to us.

2. **Raw `.proto` files.** More ergonomic ("just point at my files"),
   but the runtime needs to invoke a parser (or shell out to `protoc`)
   and handle import resolution, well-known types, and circular imports.
   Considered for v2 once v1 is stable.

3. **Go module bridge.** Helper `grpc.descriptors_from_module(path)`
   that scans a Go module path for `*.pb.go` files and reflects on
   their compiled descriptors at spec-load time. Solves the truck-api
   case directly but couples Faultbox to the Go ecosystem. Considered
   for v2.

### Multi-service / multi-port single mock

A common pattern (per truck-api Phase 1) is one mock process exposing
many gRPC services on different ports:

```python
upstreams = grpc.cluster(
    name = "upstreams",
    descriptors = "./proto/all_upstreams.pb",
    interfaces = {
        "geo_config":   ("grpc", 9001),
        "geo_facade":   ("grpc", 9002),
        "user_service": ("grpc", 9003),
        "balance_api":  ("grpc", 9004),
    },
    services = {
        "/inDriver.geo_config.GeoConfigService/*":   { ... },
        "/inDriver.geo_facade.GeoFacadeService/*":   { ... },
        "/inDriver.user.UserService/*":              { ... },
        "/inDriver.balance.BalanceService/*":        { ... },
    },
    # routes auto-bind to the correct port based on which service
    # the method belongs to (resolved from the descriptor set)
)

# Reference individual upstreams as upstreams.geo_config etc.
```

`grpc.cluster()` is a convenience over N invocations of `grpc.server()`
sharing a single descriptor set. Helps the truck-api-style "8 mocks
on 8 ports" use case without 8x boilerplate.

### Default response shape

For un-mocked methods, return `Unimplemented` (gRPC code 12) by
default — same as a real `Unimplemented*Server` would. Spec authors
override per-method as needed.

### Fault injection composition

Existing fault rules continue to apply against typed mocks unchanged:

```python
load("@faultbox/recipes/grpc.star", "grpc_faults")

geo_unstable = fault_assumption("geo_unstable",
    target = upstreams.geo_config,
    rules  = [grpc_faults.unavailable()],
)
```

`grpc_faults.unavailable()` short-circuits before the typed handler
runs, so the proxy returns the canonical UNAVAILABLE status without
ever consulting the descriptor set. No interaction concerns.

## Resolved Design Decisions

### D1 — v1 ships with FileDescriptorSet input only

Raw `.proto` parsing and Go-module bridging are deferred to v2. v1 is
about closing the gap, not solving every ingestion pattern. A `protoc`
build step is acceptable scope-creep for customers who already maintain
proto packages.

### D2 — Well-known types ship embedded

Faultbox embeds the standard `google.protobuf.*` descriptors
(`Timestamp`, `Duration`, `Empty`, `Any`, `Struct`, `Value`, `ListValue`,
field-mask, wrappers). Customer descriptor sets that import these
resolve transparently without requiring `--include_imports`.

### D3 — `grpc.dynamic(handler)` receives + returns Starlark dicts, not message objects

The handler signature is `func(request_dict) -> response_dict`. We
do not expose `dynamicpb.Message` as a Starlark value — that would
require a much larger surface and ties Starlark behavior to protobuf
semantics no spec author wants to reason about.

### D4 — Request-time validation in v1 (load-time deferred to v2)

Response-dict shape validation happens at request-time, not at
`LoadFile`. A malformed response ("unknown field `cityid`") will
surface when the SUT hits that route, not when the spec loads.

Rationale: strict load-time validation was tempting for DX, but it
requires walking every route's response dict against its descriptor
at spec-parse time, which doubles the descriptor-resolution cost
and adds a new failure mode (spec fails to load because a route's
response is wrong) before any test runs. Deferring to runtime keeps
v1 simple and matches how the untyped `mock_service` already behaves.

Re-evaluate for v2 if customers hit response-shape bugs in practice.

### D5 — Bytes escape hatch

For corner cases where the typed encoder can't express what the
customer needs (oneof tricks, deprecated fields, extensions), allow
`grpc.raw_response(bytes)` that bypasses the encoder. Power-user
escape valve, not the default path.

### D6 — TLS transparently inherited from RFC-017

Existing `mock_service(tls=True)` machinery applies to typed gRPC
mocks unchanged.

## Resolved Questions (2026-04-21)

1. **Streaming RPCs → SKIP for v1.** Only revisit if a paying customer
   asks. Truck-api's entire Phase 1 surface is unary; we have no
   evidence the added complexity (Starlark generators / iterator
   semantics / backpressure model) is worth carrying until someone
   shows us a concrete gap.

2. **Custom error details (`google.rpc.Status` + typed details)
   → DEFERRED to v2.** Plain status-code errors via `grpc.error(code)`
   cover the truck-api use case and likely 90% of early adopters.
   Add when the first customer hits a real need.

3. **Descriptor-set authoring tooling (`faultbox proto build`)
   → NO, never.** We will not ship a `protoc` wrapper and we will
   not take `protoc` as a dependency (build-time or otherwise).
   Customers maintain their own `.pb` files via their existing build
   pipeline — the inDrive monorepo pattern (a dedicated proto
   repository that publishes `.proto` + pre-generated `.pb.go`) is
   the canonical shape. The RFC + docs will describe exactly what
   Faultbox needs as input (a `FileDescriptorSet` `.pb` file, built
   with `--include_imports`), and customers deliver it.

4. **Load-time vs request-time response validation
   → request-time in v1; load-time deferred to v2.** See D4 above.

5. **Connect / connect-go protocol compatibility → VERIFY during
   implementation.** Expected to work since gRPC-Go's server accepts
   Connect clients on the unary path, but explicitly exercised with
   a connect-go client in the Phase 1 integration tests. If it
   doesn't work, document the gap; no code workaround in v1.

6. **Reflection support → YES, ship it.** When a `descriptors=`
   argument is provided, the mock server automatically registers
   the descriptor set with `grpc.reflection.v1.ServerReflection`
   so `grpcurl -plaintext localhost:9001 list` / `describe` /
   `list <service>` all work without the customer pointing grpcurl
   at a `.proto` file. Ergonomic win for debugging; ~50 LOC using
   the stdlib reflection package.

## Implementation Plan

### Phase 1 — Core typed encoder

1. Add `internal/protocol/grpc_descriptors.go`: parse a
   `FileDescriptorSet` from disk, build a `protoregistry.Files`,
   resolve message types by fully-qualified name.
2. Embed the well-known types `*.pb` into the binary (one-time build
   step from `google.golang.org/protobuf/types/known/*`).
3. Extend `protocol.MockSpec` with `Descriptors *protoregistry.Files`.
4. Replace `JSONToGRPCStruct` for typed mocks with
   `dictToTypedMessage(desc, dict)`.

### Phase 2 — Starlark surface

5. Write `@faultbox/mocks/grpc.star` exposing `grpc.server`,
   `grpc.cluster`, `grpc.dynamic`, `grpc.raw_response`.
6. Wire `descriptors=` kwarg through `mock_service()` to the
   protocol plugin.

### Phase 3 — Reflection + protocol compatibility

7. Register the provided `FileDescriptorSet` with
   `grpc.reflection.v1.ServerReflection` on every typed mock so
   `grpcurl -plaintext <addr> list` / `describe <method>` work
   out-of-the-box (resolved question 6).
8. Verify Connect / connect-go clients interoperate with the typed
   mock on the unary path; capture pass/fail as part of integration
   tests (resolved question 5). If incompatible, document the gap.

### Phase 4 — DX + release

9. Request-time validation error messages for missing methods and
   shape mismatches (must point at the right route key with the
   offending field — "unknown field `cityid` in response for
   `/pkg.S/M` (did you mean `city_id`?)").
10. Tutorial chapter: "Typed gRPC mocks" — mirrors the existing
    "Mock Services" chapter pattern.
11. Replace the Go-binary pattern in `docs/mock-services.md` with
    a pointer to the typed-Starlark approach (keep the Go-binary
    section as a permanent power-user fallback).
12. Release v0.9.0.

### Deferred to later releases (not in v0.9.0 scope)

- **Streaming RPCs** (resolved question 1 — wait for paying customer ask).
- **Custom error details / `google.rpc.Status`** (resolved question 2 — v2).
- **Load-time response-shape validation** (resolved D4 — v2).
- **Raw `.proto` ingestion** (D1 — v2).
- **`faultbox proto build` tooling** (resolved question 3 — never).

## Impact

- **Breaking changes:** None. Existing untyped `mock_service()` for
  gRPC continues to work. `grpc.server()` is opt-in.
- **Migration:** Customers using the v0.8.6 Go-binary pattern can
  delete their Go binaries and switch to `grpc.server()` once v0.9.0
  ships, but are not required to.
- **Performance:** Mock servers target low RPS. Descriptor lookup
  per-request is O(1) hashmap; not a concern.
- **Security:** New file-read at spec-load time (`descriptors=` path).
  Same trust model as `binary=` paths today.

## Alternatives Considered

- **Generate a Go mock binary from the spec.** Faultbox would emit
  Go source, compile it, and run it. Rejected — reintroduces a build
  step, defeats the "single .star file" pitch, doesn't solve the
  "I don't have proto files in this repo" case.
- **Embed protoc.** Statically-linked protoc-as-library exists
  (`pkg.go.dev/google.golang.org/protobuf/internal/protoc`) but is
  internal-only. Building our own .proto parser is too large for v1.
- **Require customers to commit `*.pb.go` and reflect on them.**
  Rejected — moves the schema source closer to Go, further from the
  customer's actual proto layout.
- **Hand-rolled descriptor JSON.** Customers handcraft JSON
  descriptors instead of using `protoc`. Rejected — `protoc` is
  ubiquitous in any codebase that uses gRPC.

## Dependencies

- RFC-017 (mock_service core) — ✅ shipped v0.8.0.
- `@faultbox/recipes/grpc.star` (status-code recipes) — ✅ shipped
  v0.8.3.
- v0.8.6 docs documenting the Go-binary workaround pattern — gives
  customers a path forward while this RFC is in development.
