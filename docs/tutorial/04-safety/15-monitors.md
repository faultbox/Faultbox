# Chapter 15: Monitors & Temporal Properties

**Duration:** 30 minutes
**Prerequisites:** [Chapter 14 (Invariants)](14-invariants.md) completed

## Goals & Purpose

Chapter 14 taught you to write invariants using assertions that run at
specific points in the test. But assertions have a timing problem: they
check at the moment you call them. If a violation happens between two
assertions, you miss it.

**Monitors** solve this. A monitor is a callback that fires on **every
event** as it arrives — syscall events, protocol events, stdout events,
WAL events, topic events. If your invariant is violated at any point
during any test, the monitor catches it immediately.

This chapter teaches you to:
- **Understand monitor lifecycle** — when they start, fire, and stop
- **Express temporal properties** as monitors
- **Combine monitors with event sources** for deep application-level verification
- **Write monitors at syscall level, protocol level, and combined**

## What is a monitor?

A monitor is a function that receives every matching event in real-time:

```python
def my_monitor(event):
    if something_bad(event):
        fail("invariant violated: " + str(event.data))

monitor(my_monitor, service="api")
```

When registered, the monitor is called for every event from the specified
service. If the function calls `fail()`, the test fails immediately with
a "monitor violation" error.

## Monitor lifecycle

### When does a monitor start?

When you call `monitor(fn, ...)`. It begins receiving events from that
moment forward.

### When does a monitor fire?

On every matching event — as the event is emitted. This is synchronous:
the event is delivered to all monitors before processing continues.

### When does a monitor stop?

At the end of the test. Each test starts with a clean set of monitors.
Monitors from one test don't carry over to the next.

### What happens when a monitor calls fail()?

The test fails **immediately**. The failure message includes which
monitor triggered it and the event that caused it. No further events
are processed for that test.

```
--- FAIL: test_my_scenario (523ms, seed=0) ---
  reason: monitor violation: stock went negative: -3
```

## Temporal properties with monitors

### Property: "A must happen before B"

```python
seen_write = {"happened": False}

def write_before_response(event):
    # Track: did the WAL write happen?
    if (event.type == "syscall" and event.service == "db"
            and event.fields.get("syscall") == "write"
            and "wal" in event.fields.get("path", "")):
        seen_write["happened"] = True

    # Check: when the response is sent, was write already done?
    if (event.type == "step_recv" and event.service == "test"):
        if not seen_write["happened"]:
            fail("response sent before WAL write!")

monitor(write_before_response)
```

This fires on every event. When it sees the HTTP response event, it
checks whether the WAL write already happened. If not — ordering violation.

### Property: "A must never happen after B"

```python
order_confirmed = {"done": False}

def no_write_after_confirm(event):
    if (event.type == "stdout" and event.service == "api"
            and event.data.get("status") == "confirmed"):
        order_confirmed["done"] = True

    if (order_confirmed["done"]
            and event.type == "syscall" and event.service == "db"
            and event.fields.get("syscall") == "write"):
        fail("DB write happened after order was confirmed to user!")

monitor(no_write_after_confirm)
```

### Property: "At most N occurrences"

```python
retry_count = {"n": 0}

def max_retries(event):
    if (event.type == "stdout" and event.service == "api"
            and event.data.get("action") == "retry"):
        retry_count["n"] += 1
        if retry_count["n"] > 3:
            fail("too many retries: " + str(retry_count["n"]) +
                 " (circuit breaker should have tripped)")

monitor(max_retries, service="api")
```

### Property: "If A happens, B must follow within N events"

```python
pending_writes = {"ids": []}

def write_must_be_fsync(event):
    # Track writes to WAL
    if (event.type == "syscall" and event.fields.get("syscall") == "write"
            and "wal" in event.fields.get("path", "")):
        pending_writes["ids"].append(event.seq)

    # When fsync happens, clear pending writes
    if (event.type == "syscall" and event.fields.get("syscall") == "fsync"):
        pending_writes["ids"] = []

    # If we have too many unsynced writes, flag it
    if len(pending_writes["ids"]) > 10:
        fail("WAL has " + str(len(pending_writes["ids"])) +
             " unsynced writes — data durability risk")

monitor(write_must_be_fsync, service="db")
```

## Monitors at the syscall level

Syscall monitors see low-level operations — writes, reads, connects,
fsyncs. They verify infrastructure-level invariants.

### Example: "No unhandled denied syscalls"

```python
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

denied_count = {"n": 0}
response_seen = {"status": 0}

def denied_syscall_handled(event):
    """If a syscall is denied, the service MUST return an error response."""
    if (event.type == "syscall" and event.service == "api"
            and event.fields.get("decision", "").startswith("deny")):
        denied_count["n"] += 1

monitor(denied_syscall_handled, service="api")

def test_denied_writes_produce_errors():
    denied_count["n"] = 0
    def scenario():
        resp = api.post(path="/data/key", body="value")
        # If any syscall was denied, the response must reflect it.
        if denied_count["n"] > 0:
            assert_true(resp.status >= 400,
                "denied syscalls but got " + str(resp.status))
    fault(db, write=deny("EIO"), run=scenario)
```

**Linux:**
```bash
faultbox test monitor-test.star --test denied_writes_produce_errors
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test monitor-test.star --test denied_writes_produce_errors"
```

## Monitors at the protocol level + event sources

Event source monitors see application-level data — log messages, database
rows, Kafka messages. They verify business-level invariants.

### Example: "No orphan Kafka events" (requires containers)

> **This example uses containers.** See [Chapter 9](../05-advanced/09-containers.md).

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image="confluentinc/cp-kafka:7.6",
    healthcheck=tcp("localhost:9092"),
    observe=[topic("order-events", decoder=json_decoder())],
)

db = service("db",
    interface("pg", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test"},
    healthcheck=tcp("localhost:5432"),
    observe=[wal_stream(tables=["orders"])],
)

# Track: event published but no corresponding DB row
published_ids = {"set": set()}
persisted_ids = {"set": set()}

def track_kafka_publish(event):
    if event.type == "topic" and event.data.get("order_id"):
        oid = event.data["order_id"]
        published_ids["set"].add(oid)

def track_db_insert(event):
    if (event.type == "wal" and event.data.get("table") == "orders"
            and event.data.get("action") == "INSERT"):
        persisted_ids["set"].add(event.data.get("order_id"))

def no_orphan_events(event):
    """Kafka event without DB row = orphan (consumer will process
    an order that doesn't exist)."""
    if event.type == "topic" and event.data.get("order_id"):
        oid = event.data["order_id"]
        if oid not in persisted_ids["set"]:
            fail("orphan Kafka event: order " + oid +
                 " published but not in DB")

monitor(track_kafka_publish)
monitor(track_db_insert, service="db")
monitor(no_orphan_events)

def test_no_orphan_on_db_failure():
    """When DB fails, no event should be published to Kafka."""
    def scenario():
        api.http.post(path="/orders", body='{"item":"widget"}')
        # Don't assert on status — we're checking the invariant
    fault(db, write=deny("EIO"), run=scenario)
```

### What the monitors verify together:

1. `track_db_insert` records every WAL INSERT to the orders table
2. `track_kafka_publish` records every Kafka message on order-events
3. `no_orphan_events` checks on every Kafka publish: is the order already
   in the DB? If not — the code published the event before confirming
   the DB write. That's a bug.

This catches the classic **dual-write** problem: publishing an event
before the database commit succeeds.

## Combined: syscall + protocol + event sources

The most powerful monitors combine all three levels:

```python
def comprehensive_order_monitor(event):
    """Tracks the full order lifecycle across all event types."""

    # Syscall level: WAL write happened
    if (event.type == "syscall" and event.service == "db"
            and event.fields.get("syscall") == "write"
            and "wal" in event.fields.get("path", "")):
        # DB is actually writing to disk — good
        pass

    # Event source level: Kafka event published
    if event.type == "topic" and event.data.get("topic") == "order-events":
        # Check: was the DB write already confirmed?
        wal_events = events(where=lambda e:
            e.type == "wal" and e.data.get("action") == "INSERT"
            and e.data.get("order_id") == event.data.get("order_id"))
        if len(wal_events) == 0:
            fail("Kafka event published before DB commit for order " +
                 event.data.get("order_id", "?"))

    # Protocol level: HTTP response sent to user
    if (event.type == "step_recv" and event.service == "test"):
        # All pending writes should be fsynced by now
        pass

monitor(comprehensive_order_monitor)
```

This single monitor watches:
- **Syscall events** — disk writes happening at the kernel level
- **WAL events** — database rows being inserted (event source)
- **Kafka events** — messages being published (event source)
- **HTTP events** — responses sent to the user (protocol)

And verifies the ordering constraint across all four.

## Shared monitors across tests

Put invariant monitors in a separate file and load them everywhere:

```python
# invariants.star
def register_order_invariants():
    """Call this at the start of any test file that involves orders."""
    monitor(no_orphan_events)
    monitor(no_negative_stock, service="inventory")
    monitor(no_duplicate_orders)
    monitor(max_retries_check, service="api")
```

```python
# any-test.star
load("invariants.star", "register_order_invariants")
register_order_invariants()

def test_happy_path():
    # All invariants active during this test
    api.post(path="/orders", body='...')

def test_db_failure():
    # Same invariants, under fault
    def scenario():
        api.post(path="/orders", body='...')
    fault(db, write=deny("EIO"), run=scenario)
```

Every test — happy path and fault — runs with the same invariants active.
If you add a new test, the invariants protect it automatically.

## What you learned

- **Monitors** fire on every event in real-time — catch violations immediately
- **Lifecycle:** start at `monitor()`, fire on each event, stop at test end
- **Temporal properties:** "A before B", "never A after B", "at most N times"
- **Syscall monitors:** infrastructure invariants (denied syscalls, write ordering)
- **Event source monitors:** business invariants (no orphan events, no duplicates)
- **Combined monitors:** cross-layer verification (syscall + WAL + Kafka + HTTP)
- **Shared monitors:** load invariants in every test file

## What's next

Chapter 16 introduces **network partitions** — bidirectional splits between
services that combine naturally with monitors to verify split-brain
behavior and partition tolerance.
