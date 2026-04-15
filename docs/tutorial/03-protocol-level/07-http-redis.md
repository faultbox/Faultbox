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

## Scenarios

Define the happy-path probes first:

```python
def write_and_read():
    """Write data, then read it back."""
    api.post(path="/data/testkey", body="hello")
    return api.get(path="/data/testkey")

scenario(write_and_read)

def health():
    return api.get(path="/health")

scenario(health)
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

Protocol-level faults use `fault_assumption()` with an interface reference
as target and `rules=` for the proxy rules:

```python
api_503 = fault_assumption("api_503",
    target = api.public,
    rules = [response(method="POST", path="/data/*", status=503,
                      body='{"error":"service unavailable"}')],
)

fault_scenario("write_gets_503",
    scenario = write_and_read,
    faults = api_503,
    expect = lambda r: assert_eq(r.status, 503, "expected 503 from proxy"),
)
```

**Linux:**
```bash
faultbox test protocol-test.star --test write_gets_503
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test protocol-test.star --test write_gets_503"
```

**What happened:**
1. Faultbox started a proxy on a random port
2. Routed `api.post(...)` through the proxy instead of directly to the API
3. The proxy matched `POST /data/*` and returned 503
4. The `expect` lambda verified the response
5. After the test, the proxy rules were cleared

**Key difference from syscall faults:**
- `target = db, write = deny("EIO")` → denies ALL write syscalls on db
- `target = api.public, rules = [response(...)]` → returns 503 only for matching HTTP requests

## HTTP: Delay specific requests

```python
slow_data = fault_assumption("slow_data",
    target = api.public,
    rules = [delay(path="/data/*", delay="500ms")],
)

fault_scenario("slow_reads",
    scenario = write_and_read,
    faults = slow_data,
    expect = lambda r: assert_true(r.duration_ms >= 400,
        "expected delay >= 400ms, got " + str(r.duration_ms)),
)
```

Notice: `delay()` without a positional argument returns a protocol-level
fault. With a positional argument (`delay("500ms")`), it returns a
syscall-level fault. Same builtin, different behavior based on usage.

## HTTP: Drop connection

```python
drop_uploads = fault_assumption("drop_uploads",
    target = api.public,
    rules = [drop(method="POST", path="/upload")],
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

### Why multiple rules?

In production, failures rarely come alone. A degraded database might respond
slowly to reads AND reject writes entirely. An overloaded upstream might
return 429 for mutations while serving stale data for queries. Testing each
fault in isolation tells you how the service handles one problem — testing
them together tells you how it behaves under realistic degradation.

Multiple rules in a single `fault()` call simulate this. Each rule matches
independently — a request that matches any rule gets that fault applied.

```python
degraded_api = fault_assumption("degraded_api",
    target = api.public,
    rules = [
        response(method="POST", path="/data/*", status=503),
        delay(method="GET", path="/data/*", delay="500ms"),
    ],
    description = "writes fail with 503, reads are slow",
)
```

Use it in the matrix alongside the single-rule assumptions:

```python
fault_matrix(
    scenarios = [write_and_read, health],
    faults = [api_503, slow_data, degraded_api],
    default_expect = lambda r: assert_true(r != None),
    overrides = {
        (write_and_read, api_503): lambda r: assert_eq(r.status, 503),
        (write_and_read, degraded_api): lambda r: assert_eq(r.status, 503),
    },
)
```

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

### Example: compose protocol + syscall fault assumptions

Use `faults=` composition to combine both levels in one assumption:

```python
# Protocol-level: proxy blocks one POST path
block_key = fault_assumption("block_key",
    target = api.public,
    rules = [response(method="POST", path="/data/key", status=503)],
)

# Syscall-level: DB writes are slow
db_slow = fault_assumption("db_slow",
    target = db,
    write = delay("200ms"),
)

# Combined: both active simultaneously
cascade = fault_assumption("cascade",
    faults = [block_key, db_slow],
    description = "proxy blocks /data/key + DB writes slow",
)

fault_scenario("cascade_test",
    scenario = write_and_read,
    faults = cascade,
    expect = lambda r: assert_eq(r.status, 503),
)
```

**The key insight:** protocol faults and syscall faults operate at different
layers and compose cleanly. The proxy matches HTTP requests (method + path),
the seccomp filter matches syscalls. They don't interfere — `faults=` merges
them into one assumption for use in `fault_scenario()` or `fault_matrix()`.

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
