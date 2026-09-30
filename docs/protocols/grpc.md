# gRPC Protocol Reference

Interface declaration:

```python
orders = service("orders",
    interface("grpc", "grpc", 50051),
    image = "myapp-orders:latest",
    healthcheck = ready(timeout = "30s"),
)
```

## Methods

### `call(method=, body={}, descriptors=)`

Invoke a unary gRPC method using a local protobuf descriptor set. Reflection
is not required. Faultbox encodes the request as the method's protobuf input
and decodes the response as its protobuf output.

```python
resp = orders.grpc.call(
    method="/orders.OrderService/CreateOrder",
    descriptors="./orders.pb",
    body={"item": "widget", "qty": 1},
    metadata={"authorization": "Bearer test-token"},
    timeout="3s",
)
assert_true(resp.ok, resp.error)
# resp.data contains the decoded response fields directly.
```

Generate descriptors with `protoc --include_imports --descriptor_set_out=orders.pb
orders.proto`. Descriptor paths are resolved relative to the Starlark module
that makes the call. Standard `google.protobuf` types resolve automatically;
include other imported schemas in the descriptor set.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `method` | string | required | Full unary gRPC path (`/package.Service/Method`) |
| `descriptors` | string | omitted | FileDescriptorSet path; enables typed requests and responses |
| `body` | dict or JSON string | `{}` | Typed request; unknown fields and invalid values fail before sending |
| `metadata` | dict | `{}` | Request metadata; values are strings or lists of strings |
| `timeout` | duration string | caller deadline | Positive RPC timeout; also respects earlier caller cancellation |
| `body_base64` | string | omitted | Explicit raw protobuf bytes; cannot combine with `body` or `descriptors` |

**Typed response:** `.data` is the decoded message using protobuf field names
(e.g. `order_id`). Integers of protobuf type `int64`/`uint64` are JSON strings,
bytes are base64, enums are names, and standard well-known types use protobuf
JSON rules. Default scalar fields are included. `.headers` contains response
metadata. `.ok` is true for gRPC status OK, `.status` contains the numeric gRPC
status (including failures), and `.duration_ms` records elapsed time.

Streaming methods are rejected. Schema, JSON, and argument errors are reported
as spec errors; server errors and RPC deadlines produce a failed response with
the real gRPC status.

**Legacy raw calls:** without `descriptors`, a string `body` remains opaque
bytes; it is not converted from JSON. An omitted body, `""`, or `"{}"` means an
empty request, preserving existing health/ping calls. Dict bodies require
`descriptors`. For arbitrary binary requests use `body_base64`. Raw responses
retain `.data["method"]` and `.data["raw"]`, and add `.data["raw_base64"]` for
lossless access to binary responses (the legacy `raw` string may replace invalid
UTF-8).

## gRPC Status Codes

| Code | Name | Description |
|------|------|-------------|
| 0 | OK | Success |
| 1 | CANCELLED | Operation cancelled |
| 2 | UNKNOWN | Unknown error |
| 3 | INVALID_ARGUMENT | Client sent invalid argument |
| 4 | DEADLINE_EXCEEDED | Timeout |
| 5 | NOT_FOUND | Resource not found |
| 13 | INTERNAL | Internal server error |
| 14 | UNAVAILABLE | Service unavailable |
| 16 | UNAUTHENTICATED | Authentication required |

## Fault Rules

### `error(method=, status=, message=)`

Return a gRPC error for matching methods.

```python
unavailable = fault_assumption("orders_unavailable",
    target = orders.grpc,
    rules = [error(method="/orders.OrderService/*", status=14,
                   message="service unavailable")],
)

not_found = fault_assumption("order_not_found",
    target = orders.grpc,
    rules = [error(method="/orders.OrderService/GetOrder", status=5,
                   message="order not found")],
)

deadline = fault_assumption("deadline_exceeded",
    target = orders.grpc,
    rules = [error(method="*", status=4, message="deadline exceeded")],
)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `method` | string | gRPC method glob (`"/orders.OrderService/*"`) |
| `status` | int | gRPC status code (see table above) |
| `message` | string | Error message |

### `delay(method=, delay=)`

```python
slow_orders = fault_assumption("slow_orders",
    target = orders.grpc,
    rules = [delay(method="/orders.OrderService/CreateOrder", delay="5s")],
)
```

A matched delay emits its `proxy` hit (`action="delay"`, `phase="started"`)
before waiting. The RPC deadline, client cancellation, or proxy shutdown ends
the wait promptly. A separate `proxy_delay_completed` or
`proxy_delay_cancelled` event records its outcome; `rpc_id` correlates these
events within the service/interface proxy. Outcome events do not count as
additional fault hits.

Leaving a fault scope clears rules for new RPCs. Already-matched RPCs retain
their delay, subject to cancellation. Clearing rules keeps the proxy listener
open; explicit test teardown emits `proxy_stopping`, cancels active RPCs, and
closes it.

### `drop(method=)`

Returns `UNAVAILABLE` with "connection dropped" message.

```python
drop_creates = fault_assumption("drop_creates",
    target = orders.grpc,
    rules = [drop(method="/orders.OrderService/CreateOrder")],
)
```

## Seed / Reset Patterns

gRPC services are typically backed by a database — seed the database
directly rather than the gRPC service:

```python
orders = service("orders",
    interface("grpc", "grpc", 50051),
    image = "myapp-orders:latest",
    depends_on = [db],
    reuse = True,
    # No seed on the gRPC service — seed the DB instead
)

db = service("postgres", ...,
    reuse = True,
    seed = lambda: db.main.exec(sql=open("./seed.sql").read()),
    reset = lambda: db.main.exec(sql="TRUNCATE orders RESTART IDENTITY CASCADE"),
)
```
