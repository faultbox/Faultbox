# Chapter 11: Event Sources & Observability

**Duration:** 25 minutes
**Prerequisites:** [Chapter 4 (Traces & Assertions)](../02-syscall-level/04-traces.md) completed

> **This chapter uses containers** for Postgres, Redis, and Kafka examples.
> See [Chapter 9: Containers](09-containers.md) for Docker setup.
> The stdout examples work with binary-mode services (no Docker needed).

## Goals & Purpose

Faultbox observes your system through **syscall interception** — it sees
every `write`, `connect`, `fsync`. This is powerful but low-level: you know
a write happened, but not *what* was written.

**Event sources** bridge this gap. They capture structured data from
external channels — service logs, database queries, message queues — and
emit it into the same trace as syscall events. You query them with the
same assertions and monitors.

This chapter teaches you to:
- **Capture service stdout** and decode it into structured events
- **Watch Postgres WAL** for data changes
- **Consume Kafka topics** for message verification
- **Poll Redis** for state changes
- **Query events with lambdas** for precise assertions

## Stdout: capturing service logs

The simplest event source. Add `observe=[stdout(...)]` to capture a
service's output:

```python
BIN = "bin/linux"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
    observe = [stdout(decoder=json_decoder())],
)

api = service("api", BIN + "/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)
```

Save as `events-test.star`.

### Happy path: verify logs exist

```python
def test_db_logs_operations():
    """Verify the DB logs SET operations to stdout."""
    api.post(path="/data/key1", body="value1")

    logs = events(where=lambda e: e.type == "stdout" and e.service == "db")
    print("db logged", len(logs), "lines")
    assert_true(len(logs) > 0, "db should produce log output")
```

**Linux:**
```bash
faultbox test events-test.star --test db_logs_operations
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test events-test.star --test db_logs_operations"
```

### What `.data` gives you

All event source events have a `.data` attribute that auto-decodes JSON
into native Starlark dicts. You never call `json.decode()`:

```python
def test_inspect_log_structure():
    """Print log structure to discover available fields."""
    api.post(path="/data/key1", body="value1")

    logs = events(where=lambda e: e.type == "stdout" and e.service == "db")
    for log in logs:
        print("fields:", log.data)
```

**Linux:**
```bash
faultbox test events-test.star --test inspect_log_structure
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test events-test.star --test inspect_log_structure"
```

Use `print()` first to see the structure, then write assertions against
specific fields.

## Decoders: parsing different log formats

### JSON decoder

For services that output structured JSON logs (most Go/Node services):

```python
observe = [stdout(decoder=json_decoder())]

# If service outputs: {"level":"INFO","msg":"SET key1 value1","op":"SET"}
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data.get("op") == "SET")
```

### Logfmt decoder

For services using logfmt (`key=value` pairs):

```python
observe = [stdout(decoder=logfmt_decoder())]

# If service outputs: level=INFO msg="SET key1 value1" op=SET
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data.get("op") == "SET")
```

### Regex decoder

For services with custom log formats:

```python
observe = [stdout(decoder=regex_decoder(
    pattern=r"(?P<timestamp>\S+) \[(?P<level>\w+)\] (?P<msg>.*)"
))]

# If service outputs: 2026-04-10T12:00:00Z [ERROR] connection refused
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data.get("level") == "ERROR")
```

Named capture groups (`?P<name>`) become fields in `.data`.

## Monitors: real-time log watching

Monitors fire on every event as it arrives — use them to catch errors
the moment they happen:

```python
def test_no_errors_in_logs():
    """Fail immediately if any service logs an ERROR."""
    def check_no_errors(event):
        if event.data.get("level") == "ERROR":
            fail("unexpected error in logs: " + event.data.get("msg", ""))

    monitor(check_no_errors, service="db")

    api.post(path="/data/key1", body="value1")
    api.get(path="/data/key1")
```

**Linux:**
```bash
faultbox test events-test.star --test no_errors_in_logs
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test events-test.star --test no_errors_in_logs"
```

### Monitor + fault: catching error handling

The real power: inject a fault AND monitor logs for the expected error
message. This verifies not just that the service returns an error, but
that it logs something useful for operators:

```python
def test_error_logged_on_write_failure():
    """When DB write fails, verify error appears in logs."""
    error_seen = {"count": 0}

    def watch_errors(event):
        if "error" in event.data.get("msg", "").lower():
            error_seen["count"] += 1

    monitor(watch_errors, service="api")

    def scenario():
        api.post(path="/data/key1", body="value1")
        assert_true(error_seen["count"] > 0,
            "API should log an error when DB write fails")

    fault(db, write=deny("EIO"), run=scenario)
```

**Linux:**
```bash
faultbox test events-test.star --test error_logged_on_write_failure
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test events-test.star --test error_logged_on_write_failure"
```

## Postgres: WAL stream events

> **Requires Docker.** See [Chapter 9](09-containers.md) for container setup.

Watch Postgres WAL (write-ahead log) for data changes in real-time:

```python
pg = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "mydb"},
    healthcheck = tcp("localhost:5432"),
    observe = [wal_stream(tables=["orders"])],
)

api = service("api",
    interface("http", "http", 8080),
    image = "myapp:latest",
    env = {"DATABASE_URL": "postgres://test@" + pg.main.internal_addr + "/mydb"},
    depends_on = [pg],
    healthcheck = http("localhost:8080/health"),
)
```

`wal_stream(tables=["orders"])` connects to Postgres logical replication
and emits an event for every INSERT, UPDATE, DELETE on the `orders` table.

```python
def test_order_persisted_to_db():
    """Verify the order actually reaches the database."""
    resp = api.http.post(path="/orders", body='{"item":"widget","qty":1}')
    assert_eq(resp.status, 201)

    # The WAL event proves the row was inserted — not just that
    # the API returned 201. It actually reached the database.
    assert_eventually(where=lambda e:
        e.type == "wal" and e.data.get("table") == "orders"
        and e.data.get("action") == "INSERT")
```

### WAL + fault: verify rollback

```python
def test_no_insert_on_payment_failure():
    """When payment service is down, order should NOT be persisted."""
    def scenario():
        resp = api.http.post(path="/orders", body='{"item":"widget","qty":1}')
        assert_true(resp.status >= 400, "should fail without payment")

        # Prove: no INSERT happened in the database.
        assert_never(where=lambda e:
            e.type == "wal" and e.data.get("table") == "orders"
            and e.data.get("action") == "INSERT")

    fault(api, connect=deny("ECONNREFUSED", label="payment down"), run=scenario)
```

**Why this matters:** the API returned an error, but did it actually
rollback the database transaction? `assert_never` on the WAL proves
no data was persisted — the strongest guarantee you can make.

## Kafka: topic consumer events

> **Requires Docker.**

Watch a Kafka topic for messages:

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    healthcheck = tcp("localhost:9092"),
    observe = [topic("order-events", decoder=json_decoder())],
)

worker = service("worker",
    interface("http", "http", 8080),
    image = "myapp-worker:latest",
    env = {"KAFKA_BROKERS": kafka.broker.internal_addr},
    depends_on = [kafka],
    healthcheck = http("localhost:8080/health"),
)
```

`topic("order-events", decoder=json_decoder())` consumes from the
`order-events` topic and decodes each message as JSON.

```python
def test_order_event_published():
    """Verify the worker publishes an event to Kafka."""
    resp = worker.http.post(path="/process", body='{"order_id": 42}')
    assert_eq(resp.status, 200)

    # The event should appear on the topic.
    assert_eventually(where=lambda e:
        e.type == "topic" and e.data.get("order_id") == 42)
```

### Topic + fault: message loss detection

```python
def test_no_orphan_events_on_db_failure():
    """If DB write fails, no event should be published to Kafka.

    This catches a common bug: publishing an event before confirming
    the database write. If the DB fails after publish, the event is
    orphaned — consumers process an order that doesn't exist.
    """
    def scenario():
        resp = worker.http.post(path="/process", body='{"order_id": 99}')
        assert_true(resp.status >= 500, "should fail on DB error")

        # No event should appear — the DB write failed.
        assert_never(where=lambda e:
            e.type == "topic" and e.data.get("order_id") == 99)

    fault(pg, write=deny("EIO", label="db down"), run=scenario)
```

## Redis: polling state

> **Requires Docker.**

Poll Redis keys for state changes:

```python
redis = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
    observe = [poll(command="GET session:active", interval="500ms")],
)
```

`poll()` runs a command at an interval and emits an event each time
the result changes:

```python
def test_session_created():
    """Verify the service creates a session in Redis."""
    resp = api.http.post(path="/login", body='{"user":"alice"}')
    assert_eq(resp.status, 200)

    assert_eventually(where=lambda e:
        e.type == "poll" and e.service == "redis"
        and e.data.get("value") != "")
```

## Combining event sources with syscall faults

The full power: fault at the syscall level, observe at the application
level. This tests the **end-to-end effect** of a low-level failure:

```python
def test_disk_fault_produces_error_log():
    """Syscall fault → does the service log a useful error?"""
    def scenario():
        api.post(path="/data/key1", body="value1")

        # Syscall fault caused the write to fail.
        # Did the service log something useful?
        assert_eventually(where=lambda e:
            e.type == "stdout" and e.service == "api"
            and "error" in e.data.get("msg", "").lower())

    fault(db, write=deny("EIO", label="disk failure"), run=scenario)
```

## Exporting events: JSON and ShiViz

Event source events are included in both export formats:

```bash
faultbox test events-test.star --output trace.json
faultbox test events-test.star --shiviz trace.shiviz
```

**JSON** — all events (syscall + stdout + WAL + topic + poll) in one file
with vector clocks, timestamps, and service attribution.

**ShiViz** — event source events appear on the same service swimlane as
syscall events. Open at https://bestchai.bitbucket.io/shiviz/ to see:
- DB syscall events AND WAL events on the same timeline
- Causal arrows between services
- The exact ordering: "the write syscall happened, then the WAL insert
  event was emitted, then the Kafka topic event appeared"

This is useful for debugging distributed transactions — did the event
publish happen before or after the database commit?

## What you learned

- `observe=[stdout(decoder=...)]` captures service output as events
- Three decoders: `json_decoder()`, `logfmt_decoder()`, `regex_decoder()`
- `.data` auto-decodes — no `json.decode()` needed
- `wal_stream(tables=[...])` watches Postgres WAL for INSERT/UPDATE/DELETE
- `topic("name", decoder=...)` consumes Kafka messages
- `poll(command="...", interval="...")` polls Redis or any command
- Event source events work with `assert_eventually`, `assert_never`, monitors
- Combine syscall faults + event sources to test end-to-end failure effects

## What's next

[Chapter 12: Named Operations](12-named-ops.md) — group related syscalls
into logical operations for cleaner fault specs.
