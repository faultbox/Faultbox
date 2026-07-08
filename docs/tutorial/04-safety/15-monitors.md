# Chapter 15: Monitors & Temporal Properties

**Duration:** 30 minutes
**Prerequisites:** [Chapter 14 (Invariants)](14-invariants.md) completed

## Goals & Purpose

Chapter 14 taught you to write invariants using assertions that run at
specific points in the test. But assertions have a timing problem: they
check at the moment you call them. If a violation happens between two
assertions, you miss it.

**Monitors** solve this. A monitor is a small state machine that fires on
**every matching event** as it arrives - syscall events, protocol events,
stdout events, WAL events, topic events. If your invariant is violated at
any point during any test, the monitor catches it immediately.

This chapter teaches you to:
- **Understand monitor lifecycle** — when they start, fire, and stop
- **Express temporal properties** as monitors
- **Combine monitors with event sources** for deep application-level verification
- **Write monitors at syscall level, protocol level, and combined**

## What is a monitor?

A monitor has four parts: a name, an event matcher (`on=`), optional
per-test memory (`state_init=` + `update=`), and a verdict (`check=`).
For every event that matches `on=`, Faultbox folds it into the state via
`update`, then calls `check`. If `check` returns `False`, the test fails
immediately.

```python
monitor("stock_never_negative",
    on         = match.event(type="stdout", service="api"),
    state_init = {"stock": 0},
    update     = lambda e, s: {"stock": int(e.stock)} if e.stock else s,
    check      = lambda e, s: s["stock"] >= 0,
)
```

`on=` is a matcher from the `match` module - `match.event(...)` selects
by `type`, `service`, and any event field; `match.any(...)` / `match.all(...)`
compose them. The monitor is called for every matching event; the moment
`check` returns `False`, the test fails with a "monitor violation" error.

## Monitor lifecycle

### When does a monitor start?

When you call `monitor(name, on=...)` at the top level, it is registered
spec-wide and receives events in every test. Passed to
`fault_assumption(monitors=)`, `fault_scenario(monitors=)`, or
`fault_matrix(monitors=)`, it is scoped to just those tests.

### When does a monitor fire?

On every matching event — as the event is emitted. This is synchronous:
the event is delivered to all monitors before processing continues.

### When does a monitor stop?

At the end of the test. `state_init` is seeded fresh for each test, so
one test's accumulated state never carries over to the next.

### What happens when check returns False?

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
monitor("write_before_response",
    on = match.any(
        # A: the WAL write on the db service.
        match.event(type="syscall", service="db", syscall="write", path="*wal*"),
        # B: the response handed back to the test driver.
        match.event(type="step_recv", service="test"),
    ),
    state_init = {"wrote": False},
    # Remember once we've seen the WAL write.
    update = lambda e, s: {"wrote": True} if e.type == "syscall" else s,
    # When the response is sent, the write must already have happened.
    check = lambda e, s: e.type != "step_recv" or s["wrote"],
)
```

This fires on every matching event. `update` flips `wrote` to `True` when
the WAL write lands; `check` only enforces anything on the response event,
where it requires the write to already be recorded. If not - ordering
violation.

### Property: "A must never happen after B"

```python
monitor("no_write_after_confirm",
    on = match.any(
        # B: the order is confirmed to the user.
        match.event(type="stdout", service="api", status="confirmed"),
        # A: a DB write.
        match.event(type="syscall", service="db", syscall="write"),
    ),
    state_init = {"confirmed": False},
    update = lambda e, s: {"confirmed": True} if e.type == "stdout" else s,
    # Once confirmed, no further DB write is allowed.
    check = lambda e, s: not (e.type == "syscall" and s["confirmed"]),
)
```

### Property: "At most N occurrences"

```python
monitor("max_retries",
    on = match.event(type="stdout", service="api", action="retry"),
    state_init = 0,
    update = lambda e, s: s + 1,
    # More than 3 retries → the circuit breaker should have tripped.
    check = lambda e, s: s <= 3,
)
```

### Property: "If A happens, B must follow within N events"

```python
monitor("write_must_be_fsync",
    on = match.any(
        # A: a WAL write increments the unsynced count.
        match.event(type="syscall", service="db", syscall="write", path="*wal*"),
        # B: an fsync clears it.
        match.event(type="syscall", service="db", syscall="fsync"),
    ),
    state_init = {"pending": 0},
    update = lambda e, s:
        {"pending": 0} if e.syscall == "fsync"
        else {"pending": s["pending"] + 1},
    # Too many unsynced writes is a data-durability risk.
    check = lambda e, s: s["pending"] <= 10,
)
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

# Infrastructure invariant: when the DB denies a write, the API must
# surface an error to the client - never a 2xx. The monitor remembers
# whether a deny was seen, then vets each API response.
denied_surfaces_error = monitor("denied_surfaces_error",
    on = match.any(
        match.event(type="syscall", service="db", decision="deny*"),
        match.event(type="step_recv", service="api"),
    ),
    state_init = {"denied": False},
    update = lambda e, s: {"denied": True} if e.type == "syscall" else s,
    # A response is only allowed to be a success if nothing was denied.
    check = lambda e, s:
        e.type != "step_recv" or not s["denied"] or int(e.status) >= 400,
)

def test_denied_writes_produce_errors():
    def scenario():
        api.post(path="/data/key", body="value")
    fault(db, write=deny("EIO"), run=scenario)
```

> **Note:** A top-level `monitor(name, on=...)` registers spec-wide and
> fires in every test. To scope a monitor to specific tests instead,
> pass it to `fault_assumption(monitors=)`, `fault_scenario(monitors=)`,
> or `fault_matrix(monitors=)`.

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
)

db = service("db",
    interface("pg", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test"},
    healthcheck=tcp("localhost:5432"),
)

# One monitor accumulates every persisted order_id (from WAL INSERT
# events) and vets every Kafka publish against that set. A publish for
# an order that was never inserted is an orphan.
no_orphan_events = monitor("no_orphan_events",
    on = match.any(
        match.event(type="wal", table="orders", action="INSERT"),
        match.event(type="topic", topic="order-events"),
    ),
    state_init = {"persisted": []},
    update = lambda e, s: (
        {"persisted": s["persisted"] + [e.order_id]}
            if e.type == "wal" else s
    ),
    # Kafka event without a matching DB row = orphan (the consumer would
    # process an order that doesn't exist).
    check = lambda e, s:
        e.type != "topic" or e.order_id in s["persisted"],
)

def test_no_orphan_on_db_failure():
    """When DB fails, no event should be published to Kafka."""
    def scenario():
        api.post(path="/orders", body='{"item":"widget"}')
        # Don't assert on status — we're checking the invariant
    fault(db, write=deny("EIO"), run=scenario)
```

### What the monitor verifies:

The single `no_orphan_events` monitor watches two event streams at once:

1. Every WAL `INSERT` on the orders table adds an `order_id` to the
   `persisted` set (via `update`).
2. Every Kafka publish on `order-events` is checked (via `check`): is the
   order already in the set? If not - the code published the event before
   confirming the DB write. That's a bug.

This catches the classic **dual-write** problem: publishing an event
before the database commit succeeds. (The `wal` and `topic` event
streams come from Postgres logical replication and the Kafka broker;
consult Chapter 9 for wiring event sources on container services.)

## Combined: syscall + protocol + event sources

The most powerful monitors combine all three levels:

A monitor's `on=` matcher can span every layer at once, and its `update`
folds the different event types into one running state:

```python
comprehensive_order_monitor = monitor("comprehensive_order",
    on = match.any(
        # Syscall level: WAL write hitting disk.
        match.event(type="syscall", service="db", syscall="write", path="*wal*"),
        # Event source level: database row inserted.
        match.event(type="wal", action="INSERT"),
        # Event source level: Kafka event published.
        match.event(type="topic", topic="order-events"),
        # Protocol level: HTTP response sent to the user.
        match.event(type="step_recv", service="test"),
    ),
    # Remember which orders were committed to the DB.
    state_init = {"committed": []},
    update = lambda e, s: (
        {"committed": s["committed"] + [e.order_id]}
            if e.type == "wal" else s
    ),
    # A Kafka publish must never precede the DB commit for its order.
    check = lambda e, s:
        e.type != "topic" or e.order_id in s["committed"],
)
```

This single monitor watches:
- **Syscall events** — disk writes happening at the kernel level
- **WAL events** — database rows being inserted (event source)
- **Kafka events** — messages being published (event source)
- **HTTP events** — responses sent to the user (protocol)

And verifies the ordering constraint across all four.

## First-class monitors

`monitor()` returns a **MonitorDef** value — a first-class object you can
store in a variable and reuse across fault assumptions, scenarios, and matrices.

```python
# Define once, reuse everywhere.
no_orphan = monitor("no_orphan",
    on = match.any(
        match.event(type="wal", action="INSERT"),
        match.event(type="topic", topic="order-events"),
    ),
    state_init = {"persisted": []},
    update = lambda e, s: (
        {"persisted": s["persisted"] + [e.order_id]}
            if e.type == "wal" else s
    ),
    check = lambda e, s:
        e.type != "topic" or e.order_id in s["persisted"],
)

max_retries = monitor("max_retries",
    on = match.event(type="stdout", service="api", action="retry"),
    state_init = 0,
    update = lambda e, s: s + 1,
    check = lambda e, s: s <= 3,
)
```

A top-level `monitor(...)` registers spec-wide and fires in every test.
Passing the same `MonitorDef` to `fault_assumption(monitors=)`,
`fault_scenario(monitors=)`, or `fault_matrix(monitors=)` scopes it to
just those tests instead of running everywhere.

## Monitors on fault assumptions

Attach monitors to `fault_assumption()` — they fire automatically in every
test that uses the assumption:

```python
# Any read reaching the DB while it is supposed to be down is a
# violation - check= returns False on the very first matching event.
no_db_traffic = monitor("no_db_traffic",
    on = match.event(type="syscall", service="db", syscall="read"),
    check = lambda e, s: False,
)

db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
    monitors = [no_db_traffic],  # active in every test using db_down
)
```

Now use it in a matrix — monitors travel with the assumption:

```python
fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [db_down, disk_full],
    monitors = [max_retries],  # matrix-wide monitor on top
)
# Every cell gets: db_down's no_db_traffic + matrix's max_retries
```

## Shared monitors across tests

Put invariant monitors in a separate file and load them everywhere:

```python
# invariants.star
no_orphan = monitor("no_orphan_events",
    on = match.any(
        match.event(type="wal", action="INSERT"),
        match.event(type="topic", topic="order-events"),
    ),
    state_init = {"persisted": []},
    update = lambda e, s: (
        {"persisted": s["persisted"] + [e.order_id]}
            if e.type == "wal" else s
    ),
    check = lambda e, s:
        e.type != "topic" or e.order_id in s["persisted"],
)

no_negative_stock = monitor("no_negative_stock",
    on = match.event(type="stdout", service="inventory"),
    check = lambda e, s: not e.stock or int(e.stock) >= 0,
)
```

```python
# any-test.star
load("invariants.star", "no_orphan", "no_negative_stock")

db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
    monitors = [no_orphan, no_negative_stock],
)

fault_matrix(
    scenarios = [order_flow],
    faults = [db_down],
)
# Every matrix cell runs with both invariant monitors active.
```

Every test that uses `db_down` — whether via `fault_scenario()` or
`fault_matrix()` — gets the invariants automatically.

## What you learned

- **Monitors** fire on every event in real-time — catch violations immediately
- **Lifecycle:** start at `monitor()`, fire on each event, stop at test end
- **First-class MonitorDef:** `monitor()` returns a value, stored in variables
- **Monitors on assumptions:** travel with `fault_assumption(monitors=)`
- **Matrix-wide monitors:** apply to every cell via `fault_matrix(monitors=)`
- **Temporal properties:** "A before B", "never A after B", "at most N times"
- **Syscall monitors:** infrastructure invariants (denied syscalls, write ordering)
- **Event source monitors:** business invariants (no orphan events, no duplicates)
- **Combined monitors:** cross-layer verification (syscall + WAL + Kafka + HTTP)

## What's next

Chapter 16 introduces **network partitions** — bidirectional splits between
services that combine naturally with monitors to verify split-brain
behavior and partition tolerance.
