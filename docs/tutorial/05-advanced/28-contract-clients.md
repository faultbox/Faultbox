# Chapter 28: Contract-Driven Clients — Call Your API Without Writing Requests

**Duration:** 20 minutes
**Prerequisites:** [Chapter 19 (OpenAPI Mocks)](19-openapi-mocks.md) or
[Chapter 18 (Typed gRPC Mocks)](18-typed-grpc-mocks.md), an OpenAPI 3.x
document or a protobuf `FileDescriptorSet`

## Goals & purpose

Chapters 18 and 19 pointed Faultbox at your contract to build a **mock** —
the thing your service *calls*. This chapter points it at a contract from
the other side, to build a **client** — the thing that calls *your* service.

Today a request in a spec looks like this:

```python
resp = api.public.post(path = "/v1/orders", body = '{"item_id": "sku-1", "qty": 2}')
resp = courier.main.call(method = "/courier.v1.CourierService/GetOrder",
                         body = '{"order_id": 42}')
```

You had to know the path template, the verb, the field names (`order_id`,
not `orderId`), and the full gRPC method path including package. Nothing
checks any of it. A typo in a path returns a 404 that looks exactly like
the failure you were testing for, and a misspelled proto field silently
marshals to a zero value.

`client()` (shipped in RFC-055) generates all of that from the contract.
But the reason to reach for it isn't only the typing — it's that a client
is a **named actor in the trace**, which changes what a failing run can
tell you.

This chapter teaches you to:

- **Generate a caller** from an OpenAPI document or a proto descriptor set.
- **Read a trace with several named callers** instead of one anonymous
  `test` lane.
- **Assert contract conformance under fault** — that a degraded service
  still returns what it published.
- **Use client calls as temporal anchors**.

## 1 · Your first client

Take the orders API from Chapter 19. Point a client at it:

```python
orders = service("orders", interface("public", "http", 8080), image = "orders:2.1")

api = client("mobile-app",
    target  = orders.public,
    openapi = "./specs/orders.yaml",
)

def test_fetch_order():
    resp = api.get_order(order_id = 42)
    assert_eq(resp.status, 200)
```

`get_order` was never declared anywhere in your spec. It came from the
document's `operationId: getOrder`, normalized to snake_case. `order_id =
42` bound to the `{orderId}` path parameter because the contract says
that's a path parameter.

Ask what else you can call:

```bash
$ faultbox inspect --clients faultbox.star
mobile-app
  target:    orders.public (http)
  contract:  openapi:./specs/orders.yaml@1.4.0
  validate:  off
  operations (2):
    create_order(body)     POST /orders
    get_order(order_id)    GET /orders/{orderId}
```

Misspell one and the error tells you what you meant:

```
client "mobile-app" has no operation "get_orders" (did you mean "get_order"?)
  2 operations available — run `faultbox inspect --clients` for the full table
```

### Naming rules

Contract names become snake_case, deterministically:

| Contract | Generated |
|---|---|
| `getOrder` | `get_order` |
| `GetOrderByID` | `get_order_by_id` |
| `ListOrdersV2` | `list_orders_v2` |
| *(no `operationId`)* `GET /orders/{orderId}/items` | `get_orders_order_id_items` |

If two operations collide, Faultbox refuses to load and names both — pass
`rename = {"getOrderV2": "fetch_order_v2"}` to say which is which.

### Declaring the contract once

If you already tell the interface where its contract lives, the client
inherits it:

```python
courier = service("courier",
    interface("main", "grpc", 9090, spec = "./proto/courier.pb"),
    image = "courier:1.4")

gcourier = client("gRPC-Courier", target = courier.main)   # no contract kwarg
```

The loader is picked by extension: `.yaml` / `.yml` / `.json` → OpenAPI,
`.pb` / `.desc` / `.protoset` → descriptor set.

## 2 · gRPC clients

Same builtin, different contract. Request-message fields become kwargs:

```python
gcourier = client("gRPC-Courier",
    target      = courier.main,
    descriptors = "./proto/courier.pb",
)

def test_courier_lookup():
    r = gcourier.get_order(order_id = 42)
    print(r.data["courier_eta"])
```

Fields encode against the *real* message type via the descriptor set — the
same machinery Chapter 18's typed mocks use, pointed the other way. A
misspelled field is caught before anything reaches the wire:

```
get_order(include_items=…, order_id=…): encode as courier.v1.GetOrderRequest:
  unknown field "order_ids" (did you mean "order_id"?)
```

If the descriptor set declares more than one service, say which:

```python
gcourier = client("gRPC-Courier",
    target       = courier.main,
    descriptors  = "./proto/all_upstreams.pb",
    grpc_service = "courier.v1.CourierService",
)
```

Omitting `grpc_service=` on a multi-service set is an error that lists the
candidates. Streaming methods are skipped — v1 is unary-only, and the rest
of the descriptor set still loads.

## 3 · Why a *named* client: reading the trace

Here's the part that isn't about typing.

Every request the test driver makes is recorded as `step_send` from a
service called `test`. Three logical callers — a mobile app, a partner
integration, an admin tool — all land in one lane. You cannot ask "did the
*partner* client see the error, or only the admin one?"

Give them names:

```python
mobile  = client("mobile-app",  target = orders.public, openapi = CONTRACT,
                 headers = {"X-Client": "ios/4.2"})
partner = client("partner-api", target = orders.public, openapi = CONTRACT,
                 headers = {"X-Client": "partner/1.0"})
admin   = client("admin-tool",  target = orders.public, openapi = CONTRACT,
                 headers = {"X-Client": "admin"})
```

Now each has its own swim lane in the report and its own vector-clock
participant in ShiViz:

```
seq  actor          event                     detail
 8   mobile-app     client_call.orders        create_order body={item_id:sku-1,qty:2}
11   mobile-app     client_return.orders      create_order → 201 (23ms)
15   gRPC-Courier   client_call.courier       get_order(order_id=1001)
17   gRPC-Courier   client_return.courier     get_order → UNAVAILABLE (11ms)
19   partner-api    client_call.orders        get_order(order_id=1001)
21   partner-api    client_return.orders      get_order → 200 (140ms)
```

You can query by caller:

```python
partner_failures = events(where = lambda e:
    e.type == "client_return" and e.client == "partner-api" and e.success == "false")
```

## 4 · Contract conformance under fault

This is the capability that doesn't exist without a client.

Your service is supposed to degrade gracefully when the courier upstream
dies. It returns 200 — but with `courier_eta` set to `null`, which its own
OpenAPI document declares non-nullable. Every consumer's deserializer
throws. No assertion you'd think to write catches this, because you'd have
to guess the field.

Turn on response validation:

```python
mobile = client("mobile-app",
    target   = orders.public,
    openapi  = "./specs/orders.yaml",
    validate = "response",
)

def test_degraded_response_still_honours_the_contract():
    with fault(courier.main, grpc_faults.unavailable()):
        resp = mobile.get_order(order_id = 1001)

        assert_eq(resp.status, 200)          # it "worked"
        assert_true(resp.contract_ok,        # ...did it?
                    "degraded response broke the contract: " + resp.contract_error)
```

Run it and the trace carries the finding as its own event:

```
#22  mobile-app  contract_violation.orders  get_order: Error at "/courier_eta": Value is not nullable
```

### The four modes

| `validate=` | Behaviour |
|---|---|
| `"off"` (default) | No checks. Identical to a hand-written step method. |
| `"request"` | Checks the outgoing request **before** sending. Raises — your test asked for something the contract doesn't describe. |
| `"response"` | Checks the response; sets `.contract_ok` / `.contract_error`, emits `contract_violation`. **Does not raise.** |
| `"strict"` | Both, and a response violation raises. |

`"response"` deliberately doesn't raise. A contract violation under fault
is usually *the finding*, not a harness error — it belongs in the trace as
evidence, with your assertions deciding what it means.

**An undeclared status counts as a violation.** If your document declares
only 200 and 404 for an operation and the service returns 503, that's the
undocumented degraded path — exactly what you want surfaced.

For gRPC, a non-OK status is an *outcome*, not a violation (your test
decides whether `UNAVAILABLE` was expected). Drift shows up instead as
unknown response fields — the server's proto is newer than your contract.

## 5 · Client calls as anchors

Client events are ordinary events, so the temporal primitives from
[Chapter 15](../04-safety/15-monitors.md) work with no new syntax:

```python
courier_failed = match.event(type = "client_return",
                             client = "gRPC-Courier",
                             operation = "get_order",
                             success = "false")

test("orders_never_drops_under_courier_failure",
    body   = drive_traffic,
    expect = always(no_dropped_orders, between = ("body_start", courier_failed)),
)
```

Before clients, the closest you could write was
`match.event(type="step_send", target="courier")` — which matches *any*
call to courier, from anywhere. Now the anchor says which caller, which
operation, and whether it failed.

## 6 · Faults, TLS, and remote targets

A client dials through the same proxy your step methods do, so everything
composes unchanged:

```python
# The fault goes on the SERVICE. The client is just a caller.
fault(orders.public, error(status = 503), run = lambda: mobile.get_order(order_id = 1))
```

TLS ([RFC-038](../../rfcs/0038-tls-aware-proxy.md)) and remote targets
([RFC-036](../../rfcs/0036-remote-services.md)) are inherited from the
interface. A client against a remote service is arguably the best use of
both features together: a real cluster pod, a contract-driven caller, no
image to distribute.

**A client is a trace actor, not a process.** It runs no code of its own,
installs no seccomp filter, and is never a fault target. Faulting one is an
error that points you at the service:

```
fault(mobile-app): a client is a caller, not a process — it has no syscalls to intercept
  faults go on the service it calls:
    fault(orders.public, response(status = 503), run = ...)   # protocol-level
    fault(orders, write = deny("EIO"), run = ...)             # syscall-level
```

## 7 · Auth and per-call overrides

Static headers apply to every call; `before=` runs per request, which is
where token minting goes:

```python
load("@faultbox/mocks/jwt.star", "jwt")

keys = jwt_keypair()

mobile = client("mobile-app",
    target  = orders.public,
    openapi = CONTRACT,
    headers = {"X-Client": "ios/4.2"},
    before  = lambda req: {
        "headers": {"Authorization": "Bearer " + jwt_sign(keys, {"sub": "user-1"})},
    },
)

# Per-call headers override both.
resp = mobile.get_order(order_id = 1, headers = {"X-Request-Id": "abc"})
```

When an operation's parameter collides with a reserved kwarg (`body`,
`headers`, `timeout`), or you'd rather paste the `operationId` straight
from the document, use the escape hatch:

```python
resp = mobile.call("getOrder", params = {"order_id": 42})
```

## Takeaways

- `client(name, target=, openapi=/descriptors=)` generates a typed caller
  from a contract. No paths, verbs, or field names in your spec.
- The **name is the point**: each client gets its own swim lane and
  vector-clock participant, so a trace says *who* called.
- `validate="response"` catches degraded-but-schema-invalid responses —
  including undeclared status codes — without you guessing the field.
  It records rather than raises, because that's usually the finding.
- Client events are ordinary events: they work as `events()` queries and as
  `always(between=)` / `eventually(anchor=)` anchors with no new syntax.
- Faults, TLS, and remote targets compose unchanged, because a client dials
  through the same proxy a step method does.
- A client is a caller, not a process. Fault the service it calls.
- `faultbox inspect --clients <spec.star>` answers "what can I call?".

Next: [Chapter 23 — Reading a Report →](23-reports.md)
