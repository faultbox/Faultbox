# RFC-055: `client()` — Contract-Driven Callers as First-Class Trace Actors

- **Status:** Implemented — all four phases shipped on `epic/rfc-055-clients`
- **Target:** v0.15.0 (candidate; independent of the v0.14.0 gVisor epic)
- **Created:** 2026-07-28
- **Accepted:** 2026-07-29 — all eight open questions resolved per strawman
- **Implemented:** 2026-07-29 — Phases 1–4 (see "What shipped" below)
- **Discussion:** [#149](https://github.com/faultbox/Faultbox/issues/149)
- **Depends on:** RFC-021 (OpenAPI ingestion — v0.9.3), RFC-023 (proto descriptor ingestion — v0.9.0), RFC-024 (proxy datapath — v0.9.5)
- **Relates to:** RFC-041 (temporal anchors), RFC-050 (`load()` traffic driver), RFC-052 (agent-first surface)

## Summary

Add a new topology entity — **`client()`** — that turns a machine-readable
API contract (OpenAPI 3.x or a protobuf `FileDescriptorSet`) into a
**named, typed caller** bound to a service interface, and makes that caller
a **first-class actor in the trace**: its own swim lane, its own vector
clock, its own event types, and its own anchors for temporal properties.

```python
courier = service("courier", image = "courier:latest",
    interface("main", "grpc", 9090, spec = "./proto/courier.pb"))

# One line replaces hand-written wire calls.
gcourier = client("gRPC-Courier", target = courier.main)

def test_courier_degrades(t):
    with fault(courier.main, grpc_unavailable()):
        r = gcourier.get_order(order_id = 42)      # generated from the contract
        assert_true(not r.ok)
```

Trace, with no extra instrumentation:

```
#12  gRPC-Courier  client_call.courier    get_order(order_id=42)          {gRPC-Courier: 3, test: 5}
#13  courier       syscall.read           allow
#14  courier       proxy_fault_applied    grpc UNAVAILABLE
#15  gRPC-Courier  client_return.courier  get_order → UNAVAILABLE (14ms)  {gRPC-Courier: 4, courier: 22}
```

Today the same test is `courier.main.call(method="/courier.v1.CourierService/GetOrder", body='{"order_id":42}')`
and appears in the trace as an anonymous `step_send.courier` from the
generic `test` lane.

## Motivation

### The spec language is contract-blind on the caller side

Faultbox already ingests API contracts — but only to *serve* them.
RFC-021 generates HTTP mock routes from OpenAPI; RFC-023 generates typed
gRPC mock responses from a `FileDescriptorSet`. Both are **callee**-side.

On the **caller** side, spec authors hand-assemble every request:

```python
resp = api.public.post(path = "/v1/orders", body = '{"item_id": "sku-1", "qty": 2}')
resp = courier.main.call(method = "/courier.v1.CourierService/GetOrder", body = '{"order_id": 42}')
```

That means the spec author must know, and keep in sync by hand: the exact
path template, the HTTP verb, required vs optional fields, the JSON field
names (`order_id` vs `orderId`), the full gRPC method path including
package. None of it is checked. A typo in a path produces a 404 that looks
exactly like the failure the test is trying to detect, and a field-name
typo in a proto request silently marshals to a zero value.

This is the same drift problem RFC-021 identified for mocks —
*"every manually-authored route is a place the mock and the real contract
can diverge silently"* — just pointed the other way. We solved it once for
the mock. The caller still has it.

### The trace can't tell you who called

Every driver-initiated call today is emitted as `step_send` / `step_recv`
with `service = "test"` (`internal/star/runtime.go:4167`). Consequences:

- **One lane for all callers.** A scenario with a mobile client, a partner
  integration, and an admin tool driving the same API renders as one
  undifferentiated `test` lane. You cannot ask "did the *partner* client
  see the error, or only the *admin* one?"
- **No causal identity.** The vector clock has a single `test` participant,
  so ShiViz shows one driver process regardless of how many logical callers
  the scenario models.
- **Anchors are positional, not semantic.** `match.event(type="step_send", target="courier")`
  matches *any* call to courier. There is no stable name for "the moment
  gRPC-Courier asked for order 42".

The user-visible symptom: reading a failing trace requires re-reading the
spec to work out which of the six `step_send.courier` events was the one
that mattered.

### Contract-aware assertions are impossible today

Because the runtime has no schema for the response, a test can only assert
on what the author hard-codes (`resp.status == 503`). It cannot assert the
much more interesting property:

> Under fault, the service still returned a response that **conforms to its
> published contract.**

Degraded responses that are *schema-invalid* — a 200 with a null in a
non-nullable field, a proto response missing a required oneof — are a
recurring real-world failure mode and are precisely what fault injection
should surface. With the contract loaded on the caller side, this becomes
a one-kwarg check (`validate = "response"`).

### Why now

- Both ingestion paths already exist in-tree and are proven
  (`internal/protocol/openapi.go`, `internal/protocol/grpc_descriptors.go`).
  This RFC is mostly *reuse and inversion*, not new parsing.
- `interface(spec = ...)` has been parsed and stored since early versions
  and is **read by nothing** (`internal/star/types.go:202`). It is a dangling
  hook that this RFC either fills or should retire.
- RFC-050 needs a traffic driver (`load(...)`). A named client is the
  natural thing to drive load *with*; shipping `load()` first would mean
  inventing an anonymous request generator and then retrofitting identity.
- RFC-052 (agent-first surface): a generated client is a typed tool
  surface. An agent writing a spec can enumerate `get_order(order_id=...)`
  instead of guessing paths and JSON field names.

### What happens if we don't

Spec authors keep transcribing contracts by hand; traces stay anonymous on
the driver side; contract-conformance-under-fault stays unassertable; and
`load()` lands with no identity model, which is much harder to add later
than to design in now.

## Current state

| Concern | Today | Where |
|---|---|---|
| OpenAPI ingestion | Implemented, mock-side only | `internal/protocol/openapi.go` (`LoadOpenAPI`, `findOperation`, `ValidateRequest`) |
| Proto descriptor ingestion | Implemented, mock-side only | `internal/protocol/grpc_descriptors.go` (`LoadDescriptorSet`, `ResolveMethod`) |
| Typed proto encoding | Implemented, mock-side only | `internal/protocol/grpc_typed_encoder.go` (`JSONToTypedMessage`) |
| Driver-initiated calls | Untyped step methods | `Runtime.executeStep` (`internal/star/runtime.go:4120`) |
| Driver identity in trace | Hard-coded `"test"` | `rt.events.Emit("step_send", "test", …)` |
| Lane assignment | Keyed on `ev.service` | `internal/report/app.js` |
| Event matching | `match.event(type=, service=, **fields)` — arbitrary fields | `internal/star/match.go:143` |
| `interface(spec=)` | Parsed, stored, **never read** | `internal/star/builtins.go:549`, `types.go:202` |

Two facts shape the design:

1. **Lanes and vector clocks are already keyed on the event's `service`
   string** — nothing requires it to be a real process. Emitting with
   `service = "<client name>"` gets a lane and a clock participant for free.
2. **`executeStep` already routes through the proxy when one is up**
   (`rt.proxyMgr.GetProxyAddr`). A client that reuses that path inherits
   protocol-level fault injection, TLS (RFC-038), and remote targets
   (RFC-036) with no new datapath.

## Proposed design

### 5.1 — The entity

```python
client(
    name,                       # required — trace identity, must be unique
    target = <InterfaceRef>,    # required — service interface to call
    openapi = "./api.yaml",     # OpenAPI 3.x document           } exactly
    descriptors = "./svc.pb",   # protoc FileDescriptorSet        } one, or
                                #   inherited from interface(spec=)
    grpc_service = "pkg.OrderService",  # required if the set declares >1 service
    base_path = "/v1",          # optional prefix prepended to OpenAPI paths
    headers = {...},            # static headers applied to every call
    before = lambda req: req,   # per-call hook (auth tokens, correlation ids)
    validate = "off",           # "off" | "request" | "response" | "strict"
    timeout = "5s",             # per-call deadline
) -> Client
```

`Client` is a Starlark `HasAttrs` value whose attributes are the operations
resolved from the contract. Attribute access on an unknown name is a
load-time-quality error with a suggestion:

```
client "gRPC-Courier" has no operation "get_orders"
  (did you mean "get_order"? — 14 operations available, see `faultbox inspect --clients`)
```

### 5.2 — Operation naming

Deterministic, documented, one canonical name per operation.

**OpenAPI.** `operationId` → snake_case (`getOrder` → `get_order`,
`GetOrderByID` → `get_order_by_id`). When `operationId` is absent —
common in real specs — synthesize from method + path:
`GET /orders/{id}/items` → `get_orders_id_items`. Path parameters
contribute their name, not a placeholder, so the synthesized name is
stable under path-template edits that only rename literals.

**gRPC.** proto method name → snake_case (`GetOrder` → `get_order`).
Service selection via `grpc_service=`; omitting it when the descriptor set
declares more than one service is a load-time error that lists candidates.

**Collisions** (two operations normalizing to the same name) are a
**load-time error** naming both source operations. We do not silently
disambiguate — a spec that can't name its own operations unambiguously
should say which one it means:

```python
gcourier = client("gRPC-Courier", target = courier.main,
    rename = {"getOrderV2": "get_order_v2"})
```

**Escape hatch.** `c.call("<raw operationId or /pkg.Svc/Method>", **params)`
invokes by contract-native name, bypassing normalization. Same tracing,
same validation.

### 5.3 — Call semantics

Parameters are passed as kwargs and bound by name against the contract.

**OpenAPI** — the generated signature partitions kwargs by the operation's
declared parameter locations:

```python
r = api.get_order(
    order_id = 42,                    # → path param {orderId}
    include = "items",                # → query param
    headers = {"X-Tenant": "acme"},   # → merged over client-level headers
)
r = api.create_order(body = {"item_id": "sku-1", "qty": 2})   # → requestBody
```

Unknown kwargs are an error (consistent with v0.13.2's strict-kwarg
direction — see `df781ca`). Missing *required* parameters are an error.
`body=` accepts a Starlark dict (JSON-encoded) or a string (sent verbatim).

**gRPC** — kwargs are request-message fields, encoded via `dynamicpb`
through the existing `JSONToTypedMessage` path. Unknown fields error with
a nearest-name suggestion (the mirror of RFC-023's D-4/Phase-4 error work).

**Return value** is the existing `Response` type, extended:

| Property | Type | Notes |
|---|---|---|
| `.status` / `.body` / `.data` / `.ok` / `.error` / `.duration_ms` | — | unchanged |
| `.operation` | `string` | canonical operation name |
| `.client` | `string` | client name |
| `.contract_ok` | `bool` | `True` when the response conformed (or validation off) |
| `.contract_error` | `string` | first conformance failure, `""` when clean |

Reusing `Response` (rather than a new `ClientResponse`) keeps every
existing assertion, recipe, and `expect_*` predicate working against client
calls unchanged.

### 5.4 — Validation modes

| `validate=` | Behaviour |
|---|---|
| `"off"` (default) | No schema checks. Byte-identical behaviour to today's step methods. |
| `"request"` | Outgoing request validated against the contract **before** it is sent; failure is a spec error (your test is wrong). |
| `"response"` | Response validated against the declared schema for its status code; failure records `contract_ok = False` and emits a `contract_violation` event — **does not** raise. |
| `"strict"` | Both, and a response violation raises. |

The `"response"` default-to-non-raising is deliberate: a contract violation
under fault is usually **the finding**, not a harness error. It should land
in the trace as evidence and let the test's own assertions decide.

Unknown status codes (a 503 the OpenAPI document never declared) count as
a conformance failure under `"response"` — that is exactly the "undocumented
degraded path" this is meant to catch.

### 5.5 — Trace model (the core of this RFC)

Two new event types, emitted with `service = <client name>`:

| Event | `event_type` | When |
|---|---|---|
| `client_call` | `client_call.<target-service>` | Immediately before the request goes out |
| `client_return` | `client_return.<target-service>` | Immediately after the response (or transport failure) |
| `contract_violation` | `contract_violation.<target-service>` | Response failed schema conformance (validate ≥ `"response"`) |

**Fields** (shared schema with `step_send`/`step_recv` wherever the concept
already exists, so report/assertion/bundle code paths need one allowlist
entry, not a parallel implementation):

```
client_call:   client, target, interface, protocol, operation, contract,
               params (truncated), path|method_path, spec, summary
client_return: client, target, operation, status_code, grpc_code,
               duration_ms, success, error, body (non-2xx only, 2KB cap),
               contract_ok, contract_error, spec, summary
```

`contract` carries the contract source identity (document path + `info.version`
for OpenAPI, file + service FQN for proto) so a trace records *which version
of the contract* the run was checked against.

**Vector clocks.** The client is a genuine participant, so the merge chain
grows one hop at each end:

```
test ──merge──► gRPC-Courier ──merge──► courier      (on call)
test ◄──merge── gRPC-Courier ◄──merge── courier      (on return)
```

The `test → client` merge on the way out is load-bearing: without it the
client's lane is causally disconnected from the test body in ShiViz and
appears as an independent process that spontaneously emits requests.

**Swim lane.** `internal/report/app.js` groups by `ev.service`, so a named
client gets its own lane with no renderer change. Lane label is the client
name; a badge distinguishes driver-side lanes from service lanes.

**Bundle downsampling.** `internal/report/report.go`'s keep-set must include
`client_call` / `client_return` / `contract_violation` alongside
`step_send` / `step_recv`. These are anchors — dropping them at the default
downsample level would defeat the feature.

### 5.6 — Anchors and temporal properties

Client events are ordinary events, so `match.event(type=, service=, **fields)`
(`internal/star/match.go:143`) matches them **with no matcher changes**:

```python
courier_asked = match.event(type = "client_call",
                            client = "gRPC-Courier", operation = "get_order")
courier_failed = match.event(type = "client_return",
                             client = "gRPC-Courier", operation = "get_order",
                             success = "false")

test("courier_recovers",
    body = drive,
    expect = [
        # RFC-041: nothing else observes a failure after the client's first success
        always(no_dropped_orders, between = (courier_asked, courier_failed)),
        eventually(lambda tr: tr.count(courier_asked) >= 3, anchor = courier_asked),
    ],
)
```

That is the property the motivation asks for: *"gRPC-Courier called
get_order, and Courier returned a failure"* is now a named, matchable,
anchorable fact rather than a positional guess.

Two ergonomic additions, both thin sugar over the above (see OQ-3):

- `match.call(client = c, operation = "get_order", ok = False)` — reads
  better than a four-kwarg `match.event`.
- `c.get_order.calls` / `.returns` — pre-built matchers hanging off the
  generated operation attribute, so an anchor is spelled once.

### 5.7 — Interaction with existing subsystems

| Subsystem | Interaction |
|---|---|
| **Proxy faults (RFC-024/034)** | Free. Client calls dial `rt.proxyMgr.GetProxyAddr(...)` exactly as `executeStep` does, so `fault(iface, http_500(), …)` applies unchanged. |
| **Syscall faults** | Unaffected. The client is not a process; it installs no filter and is never a fault target. `fault(<client>, …)` is a load-time error naming the mistake. |
| **TLS (RFC-038)** | Free — inherited from the target interface's `tls=`. |
| **Remote services (RFC-036)** | Free — a client against a remote interface dials the remote endpoint. This is arguably the *best* client use case: real cluster pod, contract-driven caller, no image needed. |
| **Mock services (RFC-017/021/023)** | Symmetric. The same OpenAPI document can drive a mock (callee) and a client (caller); a `client` → `mock_service` pair is a fully contract-checked loop useful for testing Faultbox itself. |
| **Determinism (RFC-040)** | L1-neutral. No new nondeterminism: parameters are explicit, no synthesis, no random data in v1. Contract loading happens at spec load. |
| **`faultbox inspect`** | New `--clients` section listing each client, its contract, and its generated operation table — the discoverability answer for "what can I call?". |
| **RFC-050 `load()`** | Future: `load(client = gcourier, op = "get_order", rate = "50/s")`. Identity model designed here so `load()` doesn't have to invent one. |

### 5.8 — Filling the `interface(spec=)` hook

`interface(name, protocol, port, spec=)` stores a contract path that
nothing reads. This RFC makes it live: a client whose `openapi=` /
`descriptors=` is omitted inherits the contract from its target interface,
selecting the loader by file extension (`.yaml`/`.yml`/`.json` → OpenAPI,
`.pb`/`.desc` → descriptor set) with the protocol as a cross-check.

```python
courier = service("courier", image = "courier:latest",
    interface("main", "grpc", 9090, spec = "./proto/courier.pb"))

gcourier = client("gRPC-Courier", target = courier.main)   # contract inherited
```

If this RFC is rejected, `interface(spec=)` should be **removed** — a
parsed-but-ignored kwarg is worse than no kwarg.

## Worked example

```python
load("@faultbox/recipes/grpc.star", "grpc_faults")

courier = service("courier", image = "courier:1.4",
    interface("main", "grpc", 9090, spec = "./proto/courier.pb"))

orders = service("orders", image = "orders:2.1",
    interface("public", "http", 8080, spec = "./openapi/orders.yaml"))

# Two named callers against the same API, different identities.
mobile  = client("mobile-app", target = orders.public,
                 headers = {"X-Client": "ios/4.2"}, validate = "response")
partner = client("partner-api", target = orders.public,
                 headers = {"X-Client": "partner"}, validate = "strict")

gcourier = client("gRPC-Courier", target = courier.main)

def drive(t):
    o = mobile.create_order(body = {"item_id": "sku-1", "qty": 2})
    assert_true(o.ok)

    with fault(courier.main, grpc_faults.unavailable()):
        r = gcourier.get_order(order_id = o.data["id"])
        assert_true(not r.ok)

        # Under a failing courier the order API must still honour its contract.
        status = mobile.get_order(order_id = o.data["id"])
        assert_true(status.contract_ok,
                    "degraded response violated contract: " + status.contract_error)
```

Resulting trace — three distinct driver lanes, each call self-describing:

```
seq  actor          event                          detail
 8   mobile-app     client_call.orders             create_order body={item_id:sku-1,qty:2}
 9   orders         syscall.write                  allow  /var/lib/orders/wal
11   mobile-app     client_return.orders           create_order → 201 (23ms) contract_ok=true
14   test           fault_applied                  courier.main grpc UNAVAILABLE
15   gRPC-Courier   client_call.courier            get_order(order_id=1001)
16   courier        proxy_fault_applied            grpc UNAVAILABLE
17   gRPC-Courier   client_return.courier          get_order → UNAVAILABLE (11ms)
18   mobile-app     client_call.orders             get_order(order_id=1001)
21   mobile-app     client_return.orders           get_order → 200 (140ms)
22   mobile-app     contract_violation.orders      get_order: /courier_eta: got null, want string
```

Event #22 is the finding, and it required no assertion to author — only
`validate="response"` and a contract the service already publishes.

## Impact

- **Breaking changes:** none. `client()` is a new builtin; all existing step
  methods, events, and matchers are untouched. `interface(spec=)` changes
  from ignored to meaningful, which can only affect specs that set it
  *and* declare a client that omits its own contract.
- **New top-level builtin: one.** RFC-044 rightly pushed back on namespace
  growth. The cost is accepted here because `client()` is a *topology entity*
  in the same tier as `service()` and `mock_service()`, not a variant of an
  existing primitive — and because it retires one dead kwarg. The
  alternative (§Alternatives, A2) of promoting methods onto `InterfaceRef`
  adds zero builtins but cannot express identity, which is the point.
- **Dependencies:** none new. `kin-openapi` (RFC-021) and
  `google.golang.org/protobuf` (RFC-023) are already required.
- **Performance:** contract parsing is once per client at spec load —
  same cost profile as the mock-side loaders. Per-call overhead is one
  map lookup plus, when validation is on, one schema walk. `validate="off"`
  is zero added cost over today's step methods.
- **Security:** new file reads at spec-load time (`openapi=`, `descriptors=`).
  Identical trust model to `mock_service(openapi=)`. The RFC-021 rule
  stands: `http://`/`https://` `$ref`s are rejected — `faultbox test` never
  fetches over the network at load time.
- **Trace size:** two events per call where there were two before
  (`step_send`/`step_recv` → `client_call`/`client_return`), plus a
  violation event only when one occurs. Net-neutral.

## Resolved questions (2026-07-29)

**Every strawman below was accepted as written.** The questions are kept in
their original form rather than collapsed into a decision table, because the
reasoning is what implementers need — a bare "replace" tells you nothing
about why the downstream migration in OQ-1 is the expensive half. Binding
consequences for implementation:

| OQ | Decision |
|---|---|
| 1 | Client calls **replace** `step_send`/`step_recv` for calls made through a client. Downstream consumers (report keep-set, `results.go`, context walk, ShiViz, `--normalize`) migrate in Phase 3. |
| 2 | The client **is** its own vector-clock participant. `--normalize` fingerprints change only for specs that adopt clients. |
| 3 | v1 ships **matchers only**. No `match.call(...)` / `c.op.calls` sugar until real specs show how anchors get spelled. |
| 4 | Missing `operationId` is **synthesized** deterministically per §5.2; the table is discoverable via `faultbox inspect --clients`. |
| 5 | **Stateless per call** in v1. No cookie jar, no connection reuse, no session pinning. |
| 6 | Contract validation lives **on the client** in v1. Proxy-side contract checking is a follow-up RFC. |
| 7 | Auth is **`headers=` + `before=`** only. `before=` composed with `jwt_sign()` covers bearer tokens. |
| 8 | A client name colliding with a service name is a **hard load-time error**. |

1. **Should client calls *replace* or *coexist with* `step_send`/`step_recv`?**
   Strawman: replace, for calls made through a client — one call, one pair of
   events. Emitting both doubles the trace and creates two truths. But every
   downstream consumer (report keep-set, `results.go` failure context at
   `internal/star/results.go:575`, `builtins.go:1131` context walk, ShiViz,
   `--normalize` goldens) has `step_send`/`step_recv` hard-coded and needs a
   coordinated update. Alternative: keep the existing types and add a
   `client=` field. Cheaper, but then the lane is still `test` unless we also
   change the emitted `service`, which is the more invasive half anyway.

2. **Is the client its own vector-clock participant, or a labelled sub-lane
   of `test`?** Strawman: own participant (§5.5). It makes ShiViz show what
   is actually being modelled — N logical callers. Cost: `--normalize`
   fingerprints change for any spec adopting clients (opt-in, so no existing
   spec breaks), and clock maps grow by one entry per client.

3. **Ship `match.call(...)` and `c.op.calls` sugar in v1, or matchers only?**
   Strawman: matchers only in v1 (they already work), sugar in v1.1 once we
   see how anchors are actually spelled in real specs. Avoids adding surface
   we might get wrong.

4. **OpenAPI operation naming when `operationId` is absent.** The synthesis
   rule in §5.2 is deterministic but produces long names
   (`get_orders_id_items`). Alternative: require `operationId` and error
   otherwise — cleaner names, but rejects a large fraction of real specs.
   Strawman: synthesize, and surface the generated table in
   `faultbox inspect --clients` so names are discoverable rather than guessed.

5. **Statefulness.** Cookie jar, connection reuse, HTTP/2 session pinning?
   Strawman: stateless per call in v1 (matches today's step methods).
   Session affinity matters for realistic client modelling and for RFC-050
   load, but it interacts with the proxy's connection accounting
   (`proxy_conn_open`/`proxy_conn_close`) and deserves its own pass.

6. **Does `validate="response"` belong on the client or on the service?**
   Contract conformance is arguably a property *of the service*, and
   declaring it once on `interface(spec=)` would check it for all callers
   including SUT-internal traffic through the proxy. Strawman: client-side
   in v1 (that's where the response is decoded and where the schema is
   already loaded); proxy-side contract checking is a strictly larger
   feature and a good follow-up RFC.

7. **Auth beyond `headers=` + `before=`.** OAuth2 client-credentials flows,
   mTLS client certs, token refresh. Strawman: out of scope for v1;
   `before=` composed with the existing `jwt_sign()` builtin covers the
   common bearer-token case. Revisit on first customer ask.

8. **Client-name collisions with service names.** Both occupy the same lane
   and clock namespace. Strawman: hard load-time error. Cheap, and the
   alternative (silent aliasing of two actors into one lane) is a debugging
   nightmare.

## Implementation plan

### Phase 1 — Contract → operation table (no Starlark surface)

1. `internal/protocol/openapi_client.go` — walk `OpenAPISpec.Doc` into an
   `[]Operation{Name, Method, PathTemplate, Params, RequestSchema, ResponsesByStatus}`.
   Reuses the existing loader; adds naming + collision detection.
2. `internal/protocol/grpc_client.go` — walk a `protoregistry.Files` into the
   same `Operation` shape; typed unary invoke plus response→dict decoding
   (the inverse of `JSONToTypedMessage`).
3. Response conformance checking for both (OpenAPI: schema-by-status;
   proto: decode success + unknown-field report).

### Phase 2 — Starlark surface

4. `internal/star/client.go` — `ClientVal` (`HasAttrs`), `OperationVal`
   (callable), kwarg→parameter binding, near-miss suggestions.
5. `internal/star/client_builtins.go` — the `client()` builtin, contract
   selection, `interface(spec=)` inheritance, name-collision validation.
6. `Response` gains `.operation` / `.client` / `.contract_ok` / `.contract_error`.

### Phase 3 — Trace integration

7. Factor the emit half of `executeStep` into a shared helper so
   `step_send`/`step_recv` and `client_call`/`client_return` have one
   source of truth for field construction and truncation.
8. Client-aware clock merges (§5.5); `contract_violation` emission.
9. Report: lane label + badge, drill-down rows for operation/params/
   contract result, keep-set entries in `internal/report/report.go`.
10. Failure-context walk (`internal/star/builtins.go:1119`) and
    `results.go:575` learn the new types.

### Phase 4 — DX + docs

11. `faultbox inspect --clients` — per-client operation table.
12. `docs/spec-language.md`: `Client` type reference, `client()` section,
    new event types in the Trace Output table.
13. Tutorial chapter mirroring the mock-services chapter.
14. `poc/` example spec exercising an OpenAPI client and a gRPC client
    against the existing demo topology.

### Explicitly out of scope for v1

- **Streaming RPCs** (mirrors RFC-023 resolved-question 1 — wait for a
  concrete ask).
- **Load generation / concurrency** — belongs to RFC-050's `load()`.
- **Request-data synthesis / fuzzing** — would introduce nondeterminism
  that needs a seeding story; explicit params only in v1.
- **Other contract formats** — GraphQL, AsyncAPI, Avro, Thrift, WSDL.
- **Client-side retry / backoff / circuit-breaker emulation.** Tempting
  (it is how real clients behave under fault) but it is a *behaviour*
  model, not a contract model, and would blur what the trace attributes
  to the SUT versus the harness.
- **Recording real traffic into a client** (HAR/pcap → replay).

## Alternatives considered

**A1 — Do nothing; keep hand-written step calls.** Rejected: it is the
status quo the motivation describes, and it leaves `interface(spec=)` dead.

**A2 — Promote generated methods onto `InterfaceRef` instead of a new
entity.** `interface("main", "grpc", 9090, spec="./courier.pb")` would make
`courier.main.get_order(...)` work with zero new builtins — genuinely
attractive on namespace grounds. Rejected because it cannot express *caller
identity*: there is one interface but N logical callers, and the whole
trace-clarity half of this RFC depends on distinguishing them. It also
conflates "what this service offers" with "who is calling it". Note the two
are compatible — if demand appears, `InterfaceRef` promotion can be added
later as sugar for an implicit default client.

**A3 — Code-generate a `.star` client module from the contract
(`faultbox generate client`).** Rejected on the same grounds RFC-021
rejected generated route tables: it reintroduces the drift the feature
exists to close, and it adds a build step before `faultbox test`.

**A4 — Wrap an existing client generator (openapi-generator, protoc plugins)
and shell out.** Rejected: third-party runtime dependency, per-language
toolchains, and it produces code in a language the spec can't call anyway.

**A5 — Keep `step_send`/`step_recv` and add a `client=` field only.**
Cheapest possible change, and it does give per-client filtering. Rejected as
the primary design because the lane and the vector clock both key on
`service`, so the trace would still show every client in the `test` lane —
which is the actual complaint. Retained as the fallback in OQ-1 if the
downstream-consumer migration proves larger than estimated.

**A6 — Model the client as a real process (a spawned binary under seccomp).**
Maximally faithful — the client could then be faulted itself. Rejected for
v1: it requires shipping a generic contract-driven client *binary*, cross-
compiling it per platform, and it drags in the whole container/binary
lifecycle for something whose value is the trace identity, not the isolation.
Revisit only if someone needs to inject syscall faults into the caller.

## Tests

- `internal/protocol/openapi_client_test.go` — naming (operationId,
  camelCase, missing-id synthesis, collisions), parameter partitioning
  (path/query/header/body), required-param enforcement, response
  conformance by status, undeclared-status handling.
- `internal/protocol/grpc_client_test.go` — method resolution, field
  encoding/decoding round-trip, unknown-field suggestion, multi-service
  descriptor set requiring `grpc_service=`.
- `internal/star/client_test.go` — `client()` load-time validation
  (contract inheritance from `interface(spec=)`, name collisions with
  services, both-contracts-supplied error, syscall-fault targeting refusal),
  attribute suggestions, kwarg binding errors.
- `internal/star/client_trace_test.go` — event emission (types, fields,
  `service` = client name), vector-clock merge chain, `contract_violation`
  emission, matcher/anchor integration with `always`/`eventually`.
- `internal/report/` — keep-set retention of client events at each
  downsample level; lane assignment.
- End-to-end: a `poc/` spec driving a `mock_service(openapi=)` with a
  `client(openapi=)` over the *same* document, under a proxy fault, asserting
  the contract violation is detected.

## What shipped

| Phase | Landed |
|---|---|
| 1 | `internal/protocol/client_contract.go` (OperationTable, naming, collisions, suggestions), `openapi_client.go` (operation walk, request binding, request/response conformance), `grpc_client.go` (descriptor walk, dynamicpb encode, unary invoke, typed decode). `http.go` accepts HEAD/OPTIONS and reports the response content type. |
| 2 | `internal/star/client.go` (ClientVal, OperationVal, call path, `before=` hook, event emission), `client_builtins.go` (the builtin, contract resolution, name validation). `Response` gains `.client` / `.operation` / `.contract_ok` / `.contract_error`. `registerService` returns an error so names can't collide. |
| 3 | `report.go` anchorTypes, `app.js` (`isCallEvent`/`isSendEvent`/`isRecvEvent` replacing ten inline checks), `results.go` normalized trace, `recentAssertionContext`, dotted `event_type` in `events.go`. |
| 4 | `faultbox inspect --clients`, `docs/spec-language.md` §Contract-Driven Clients, `docs/cli-reference.md`, `poc/client-rfc055/`. |

### Deviations from the design above

- **`events()` needed widening.** §5.6 assumed client events would be
  queryable. They were not: `events()` filtered to a hard-coded four types
  (`syscall`, `stdout`, `topic`, `wal`), so `step_send`/`step_recv` had
  never been visible to it either. Client types were added;
  `step_send`/`step_recv` deliberately were not, because admitting them
  would silently change what `events()` returns for every spec written
  before now.
- **A pre-existing path bug surfaced.** `mock_service(openapi=)` and
  `(descriptors=)` resolved against the process CWD, not the spec
  directory, so `"./api.yaml"` only worked when `faultbox` ran from the
  spec's own directory. Fixed to match `load_file()`, `build=`, and the
  new `client()`.
- **`grpcProtocol.ExecuteStep` was already non-functional for typed
  services.** It invokes with raw `[]byte` against grpc-go's proto codec,
  which only accepts `proto.Message`. Clients do not go through it; they
  invoke with dynamicpb messages, which is what makes typed calls work.
  The existing `call()` step method is left as-is — untangling it is not
  this RFC's business, but it should not be mistaken for a working path.
- **`before=` is HTTP-only.** The hook reads `headers` and `body` off the
  returned dict; gRPC calls take client-level and per-call headers but do
  not run the hook. No design reason, just unbuilt — the auth use case the
  hook exists for is HTTP-shaped.
- **Reserved-parameter collisions are load-time errors, not silent
  renames.** A contract parameter normalizing to `body`, `headers`, or
  `timeout` can't be bound by name, so the table build fails and points at
  `client.call(name, params={...})`. This wasn't spelled out in §5.3 and is
  the reason `call()` takes an explicit `params=` dict.

## References

- RFC-017 (Native Mock Services) — the callee-side entity this mirrors.
- RFC-021 (OpenAPI-Driven Mock Generation) — OpenAPI loader reused here.
- RFC-023 (Typed-Proto gRPC Mocks) — descriptor loader + typed encoder reused here.
- RFC-024 / RFC-034 (Proxy datapath & observability) — the fault path client calls traverse.
- RFC-036 (Remote Services) — highest-value client target.
- RFC-041 (Temporal Properties) — the anchor machinery client events plug into.
- RFC-044 (Spec Language Simplification) — the namespace-growth bar this RFC argues against.
- RFC-050 (Gray & Metastable Faults) — `load()` consumer of the client identity model.
- RFC-052 (Agent-First Surface) — generated operations as an agent-facing tool surface.
