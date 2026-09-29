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

### `call(method="", body="{}")`

Invoke a gRPC method with a JSON-encoded request body.

```python
resp = orders.grpc.call(
    method="/orders.OrderService/CreateOrder",
    body='{"item":"widget","qty":1}',
)
# resp.data = {"method": "/orders.OrderService/CreateOrder", "raw": "..."}

resp = orders.grpc.call(
    method="/health.HealthService/Check",
    body="{}",
)
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `method` | string | required | Full gRPC method path (`/package.Service/Method`) |
| `body` | string | `"{}"` | JSON-encoded request message |

**Response:**

| Field | Type | Description |
|-------|------|-------------|
| `.data["method"]` | string | gRPC method called |
| `.data["raw"]` | string | Raw response bytes as string |
| `.ok` | bool | `True` if gRPC status is OK |
| `.status` | int | gRPC status code (0=OK, see table below) |
| `.duration_ms` | int | Execution time |

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
