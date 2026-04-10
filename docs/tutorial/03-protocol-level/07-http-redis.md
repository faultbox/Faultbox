# Chapter 7: HTTP Protocol Faults

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

Create `protocol-test.star` in the demo directory:

```python
# Linux (native): BIN = "bin"
# macOS (Lima): BIN = "bin/linux"
BIN = "bin/linux"

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

## Happy path first

Before injecting faults, verify the system works:

```python
def test_happy_path():
    """Verify API works normally."""
    resp = api.post(path="/data/testkey", body="hello")
    assert_eq(resp.status, 200)

    resp = api.get(path="/data/testkey")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "hello")
```

Run it:

**Linux:**
```bash
faultbox test protocol-test.star --test happy_path
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test happy_path"
```

```
--- PASS: test_happy_path (200ms, seed=0) ---
1 passed, 0 failed
```

Good — the system works. Now break it.

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

**Linux:**
```bash
faultbox test protocol-test.star --test api_returns_503
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test api_returns_503"
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

**Linux:**
```bash
faultbox test protocol-test.star --test slow_data_endpoint
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test slow_data_endpoint"
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

**Linux:**
```bash
faultbox test protocol-test.star --test connection_drop
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test connection_drop"
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

### Why multiple rules?

In production, failures rarely come alone. A degraded database might respond
slowly to reads AND reject writes entirely. An overloaded upstream might
return 429 for mutations while serving stale data for queries. Testing each
fault in isolation tells you how the service handles one problem — testing
them together tells you how it behaves under realistic degradation.

Multiple rules in a single `fault()` call simulate this. Each rule matches
independently — a request that matches any rule gets that fault applied.

```python
def test_mixed_faults():
    """Writes fail with 503, reads are slow — a degraded but alive upstream."""
    def scenario():
        # Write path: should fail fast with 503.
        resp = api.post(path="/data/key", body="value")
        assert_eq(resp.status, 503)

        # Read path: still works, but slow.
        resp = api.get(path="/data/key")
        assert_true(resp.duration_ms >= 400,
            "reads should be delayed, got " + str(resp.duration_ms) + "ms")

        # Health endpoint: unaffected (no matching rule).
        resp = api.get(path="/health")
        assert_true(resp.duration_ms < 100, "health should be fast")
    fault(api.public,
        response(method="POST", path="/data/*", status=503),
        delay(method="GET", path="/data/*", delay="500ms"),
        run=scenario,
    )
```

**Linux:**
```bash
faultbox test protocol-test.star --test mixed_faults
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test mixed_faults"
```

**What happened here:**

1. Faultbox installed two proxy rules simultaneously
2. `POST /data/key` matched the `response(method="POST", ...)` rule → proxy returned 503 immediately, the real API never saw the request
3. `GET /data/key` matched the `delay(method="GET", ...)` rule → proxy waited 500ms, then forwarded to the real API
4. `GET /health` matched neither rule → proxy forwarded directly, no delay

**The key insight:** rules are independent filters. The proxy checks each
incoming request against all rules and applies the first match. Unmatched
requests pass through unmodified. This lets you simulate partial degradation
— some operations fail, others slow down, the rest work normally.

## Combining syscall + protocol faults

### Why combine both levels?

Protocol faults and syscall faults test different things:

- **Protocol faults** simulate upstream failures — "the database returned an error for this query"
- **Syscall faults** simulate local failures — "the disk is full, writes fail"

In production, these happen together. Your database starts rejecting
writes (protocol-level), and your API also can't write to its own log
or cache (syscall-level). Testing each in isolation only tells you half
the story. Testing them together tells you if the service degrades
gracefully or falls apart.

### Example: proxy blocks one request, syscall fault slows another

This test applies two faults at different levels simultaneously. Two
requests exercise different fault paths — the first hits the protocol
fault, the second hits the syscall fault.

1. **Protocol fault** — proxy returns 503 for `POST /data/key`
2. **Syscall fault** — DB write syscalls are delayed 200ms

```python
def test_cascade():
    """Protocol fault blocks one POST, syscall fault slows DB on another.

    Two requests in one test:
    - POST /data/key → intercepted by proxy, returns 503 immediately
    - POST /data/other → passes through proxy (different path), reaches
      the API, which writes to the DB. The DB write is delayed 200ms
      by the syscall fault.
    """
    def scenario():
        # Request 1: matches proxy rule → 503 (proxy intercepts, fast).
        resp = api.post(path="/data/key", body="value")
        assert_eq(resp.status, 503, "should get 503 from proxy")

        # Request 2: doesn't match proxy rule → goes through to API → DB.
        # DB write is delayed 200ms by the syscall fault.
        resp = api.post(path="/data/other", body="setup")
        assert_eq(resp.status, 200, "should succeed but be slow")

    def with_slow_db():
        fault(db, write=delay("200ms"), run=scenario)

    fault(api.public,
        response(method="POST", path="/data/key", status=503),
        run=with_slow_db,
    )
```

**Linux:**
```bash
faultbox test protocol-test.star --test cascade
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test cascade"
```

```
--- PASS: test_cascade (416ms, seed=0) ---
  syscall trace (2 events):
    #12  db    write    delay(200ms)  socket:[38131]  (+200ms)
  fault rule on db: write=delay() → filter:[write,writev,pwrite64] (1 hits)
```

**What happened here:**

```
              Protocol fault              Syscall fault
              (POST /data/key → 503)      (write → delay 200ms)
                    ↓                          ↓
Caller → Faultbox Proxy → API process → mock-db
```

1. The outer `fault(api.public, response(...))` started a proxy that intercepts HTTP requests to the API
2. The inner `fault(db, write=delay("200ms"))` installed a seccomp filter on the DB that delays all write syscalls
3. `POST /data/key` matched the proxy rule → proxy returned 503 immediately. The API and DB never saw this request
4. `POST /data/other` did NOT match the proxy rule (path is `/data/other`, not `/data/key`) → passed through to the API → API forwarded to DB → DB's write syscall was delayed 200ms
5. Both faults fired during the same test, but on different requests

**The key insight:** protocol faults and syscall faults operate at different
layers and compose cleanly. The proxy matches HTTP requests (method + path),
the seccomp filter matches syscalls. They don't interfere — you can stack
them to simulate scenarios where different parts of the system degrade
differently.

> **When to use this:** Test cascading failures where your service faces
> multiple simultaneous problems:
> - API gateway rejecting some writes + slow database (partial degradation)
> - Network partition to one dependency + disk fault on another
> - Rate limiting specific endpoints + timeout on a downstream service

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
