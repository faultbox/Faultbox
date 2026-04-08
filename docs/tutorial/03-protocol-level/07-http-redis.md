# Chapter 7: HTTP & Redis Protocol Faults

**Duration:** 25 minutes
**Prerequisites:** [Chapter 3 (Fault Injection)](../02-syscall-level/03-fault-injection.md) completed

## Goals & Purpose

Syscall-level faults are powerful but coarse — `write=deny("EIO")` breaks
ALL writes. In production, you need finer control:

- "Return 429 for `POST /orders` but let `GET /health` pass"
- "Simulate Redis returning READONLY for SET commands"
- "Delay only requests matching a specific path"

**Protocol-level faults** see inside the traffic. A transparent proxy sits
between services, speaks the real protocol, and injects faults based on
request content.

This chapter teaches you to:
- **Inject HTTP responses** — return specific status codes and bodies
- **Inject Redis errors** — fail specific commands on specific keys
- **Delay specific requests** — slow down matching paths or commands
- **Drop connections** — simulate network failures at the request level

## How it works

```
Service A ──→ Faultbox Proxy ──→ Service B
                   ↑
            Rules: match request → inject fault
```

Faultbox inserts a proxy transparently. The service doesn't know it's
there — it connects to the same address. The proxy inspects each request,
checks rules, and either injects a fault or forwards normally.

## Setup

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", BIN + "/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)
```

Save as `protocol-test.star`.

## HTTP: Return a specific status code

The `fault()` builtin works at both levels. When the first argument is
an **interface reference** (like `api.public`), it injects protocol-level
faults via a transparent proxy.

```python
def test_api_returns_503():
    """Proxy returns 503 for POST requests to /data/*."""
    def scenario():
        resp = api.post(path="/data/testkey", body="value")
        assert_eq(resp.status, 503, "expected 503 from proxy")
    fault(api.public,
        response(method="POST", path="/data/*", status=503,
                 body='{"error":"service unavailable"}'),
        run=scenario,
    )
```

Run it:
```bash
# Linux:
bin/faultbox test protocol-test.star --test api_returns_503
# macOS (Lima):
vm bin/linux-arm64/faultbox test protocol-test.star --test api_returns_503
```

**What happened:**
1. Faultbox started a proxy on a random port
2. Routed `api.post(...)` through the proxy instead of directly to the API
3. The proxy matched `POST /data/*` and returned 503
4. After the test, the proxy rules were cleared

**Key difference from syscall faults:**
- `fault(db, write=deny("EIO"))` → denies ALL write syscalls on db
- `fault(api.public, response(path="/data/*", status=503))` → returns 503 only for matching HTTP requests

## HTTP: Delay specific requests

```python
def test_slow_data_endpoint():
    """Delay GET /data/* by 500ms — other endpoints are fast."""
    def scenario():
        resp = api.get(path="/data/mykey")
        assert_true(resp.duration_ms >= 400,
            "expected delay >= 400ms, got " + str(resp.duration_ms))

        # Health endpoint is NOT delayed.
        resp2 = api.get(path="/health")
        assert_true(resp2.duration_ms < 100, "health should be fast")
    fault(api.public,
        delay(path="/data/*", delay="500ms"),
        run=scenario,
    )
```

Notice: `delay()` without a positional argument returns a protocol-level
fault. With a positional argument (`delay("500ms")`), it returns a
syscall-level fault. Same builtin, different behavior based on usage.

## HTTP: Drop connection

```python
def test_connection_drop():
    """Proxy drops the connection for POST /upload."""
    def scenario():
        resp = api.post(path="/upload", body="large data")
        assert_true(resp.ok == False, "expected connection failure")
    fault(api.public,
        drop(method="POST", path="/upload"),
        run=scenario,
    )
```

## Request matching

All protocol fault builtins support glob patterns:

| Pattern | Matches |
|---------|---------|
| `/data/*` | `/data/key1`, `/data/key2` |
| `/api/*/orders` | `/api/v1/orders`, `/api/v2/orders` |
| `POST` | Only POST method |
| `*` (or omit) | Everything |

## Multiple rules

Apply multiple rules in one `fault()` call:

```python
def test_mixed_faults():
    """503 for writes, delay for reads."""
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_eq(resp.status, 503)

        resp = api.get(path="/data/key")
        assert_true(resp.duration_ms >= 400)
    fault(api.public,
        response(method="POST", path="/data/*", status=503),
        delay(method="GET", path="/data/*", delay="500ms"),
        run=scenario,
    )
```

## Combining syscall + protocol faults

You can nest both levels:

```python
def test_cascade():
    """API gets 503 from upstream AND can't write to disk."""
    def scenario():
        def inner():
            resp = api.post(path="/data/key", body="value")
            assert_true(resp.status >= 500)
        fault(api, write=deny("ENOSPC"), run=inner)
    fault(api.public,
        response(method="POST", path="/data/*", status=503),
        run=scenario,
    )
```

## What you learned

- `fault(service.interface, ...)` injects protocol-level faults
- `response(method=, path=, status=, body=)` returns a custom HTTP response
- `delay(path=, delay=)` slows down matching requests
- `drop(method=, path=)` closes the connection
- Glob patterns match requests by method and path
- Syscall and protocol faults can be combined

## What's next

HTTP is just one protocol. Chapter 8 shows how to inject faults into
databases (Postgres, MySQL) and message brokers (Kafka, Redis) — return
query errors, drop messages, delay specific commands.
