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
def post_data():
    api.post(path="/data/key1", body="value1")
    logs = events(where=lambda e: e.type == "stdout" and e.service == "db")
    print("db logged", len(logs), "lines")
    return logs
scenario(post_data)

fault_scenario("db_logs_operations",
    scenario = post_data,
    faults = [],
    expect = lambda r: assert_true(len(r) > 0, "db should produce log output"),
)
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
def inspect_logs():
    api.post(path="/data/key1", body="value1")
    logs = events(where=lambda e: e.type == "stdout" and e.service == "db")
    for log in logs:
        print("fields:", log.data)
    return logs
scenario(inspect_logs)
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

# In an expect lambda or assertion:
# If service outputs: {"level":"INFO","msg":"SET key1 value1","op":"SET"}
# assert_eventually(where=lambda e:
#     e.type == "stdout" and e.data.get("op") == "SET")
```

### Logfmt decoder

For services using logfmt (`key=value` pairs):

```python
observe = [stdout(decoder=logfmt_decoder())]

# If service outputs: level=INFO msg="SET key1 value1" op=SET
# assert_eventually(where=lambda e:
#     e.type == "stdout" and e.data.get("op") == "SET")
```

### Regex decoder

For services with custom log formats:

```python
observe = [stdout(decoder=regex_decoder(
    pattern=r"(?P<timestamp>\S+) \[(?P<level>\w+)\] (?P<msg>.*)"
))]

# If service outputs: 2026-04-10T12:00:00Z [ERROR] connection refused
# assert_eventually(where=lambda e:
#     e.type == "stdout" and e.data.get("level") == "ERROR")
```

Named capture groups (`?P<name>`) become fields in `.data`.

## Monitors: real-time log watching

Monitors fire on every event as it arrives — use them to catch errors
the moment they happen:

```python
def post_and_get():
    api.post(path="/data/key1", body="value1")
    api.get(path="/data/key1")
scenario(post_and_get)

fault_scenario("no_errors_in_logs",
    scenario = post_and_get,
    faults = [],
    expect = lambda r: assert_never(where=lambda e:
        e.type == "stdout" and e.service == "db"
        and e.data.get("level") == "ERROR"),
)
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

The real power: inject a fault AND check logs for the expected error
message. This verifies not just that the service returns an error, but
that it logs something useful for operators:

```python
def post_data_simple():
    return api.post(path="/data/key1", body="value1")
scenario(post_data_simple)

db_write_fail = fault_assumption("db_write_fail",
    target = db,
    write = deny("EIO"),
)

fault_scenario("error_logged_on_write_failure",
    scenario = post_data_simple,
    faults = db_write_fail,
    expect = lambda r: assert_eventually(where=lambda e:
        e.type == "stdout" and e.service == "api"
        and "error" in e.data.get("msg", "").lower()),
)
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
def create_order():
    return api.http.post(path="/orders", body='{"item":"widget","qty":1}')
scenario(create_order)

fault_scenario("order_persisted_to_db",
    scenario = create_order,
    faults = [],
    expect = lambda r: [
        assert_eq(r.status, 201),
        # The WAL event proves the row was inserted — not just that
        # the API returned 201. It actually reached the database.
        assert_eventually(where=lambda e:
            e.type == "wal" and e.data.get("table") == "orders"
            and e.data.get("action") == "INSERT"),
    ],
)
```

### WAL + fault: verify rollback

```python
payment_down = fault_assumption("payment_down",
    target = api,
    connect = deny("ECONNREFUSED", label="payment down"),
)

fault_scenario("no_insert_on_payment_failure",
    scenario = create_order,
    faults = payment_down,
    expect = lambda r: [
        assert_true(r.status >= 400, "should fail without payment"),
        # Prove: no INSERT happened in the database.
        assert_never(where=lambda e:
            e.type == "wal" and e.data.get("table") == "orders"
            and e.data.get("action") == "INSERT"),
    ],
)
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
def process_order():
    return worker.http.post(path="/process", body='{"order_id": 42}')
scenario(process_order)

fault_scenario("order_event_published",
    scenario = process_order,
    faults = [],
    expect = lambda r: [
        assert_eq(r.status, 200),
        # The event should appear on the topic.
        assert_eventually(where=lambda e:
            e.type == "topic" and e.data.get("order_id") == 42),
    ],
)
```

### Topic + fault: message loss detection

```python
def process_order_99():
    return worker.http.post(path="/process", body='{"order_id": 99}')
scenario(process_order_99)

db_down = fault_assumption("db_down",
    target = pg,
    write = deny("EIO", label="db down"),
)

fault_scenario("no_orphan_events_on_db_failure",
    scenario = process_order_99,
    faults = db_down,
    expect = lambda r: [
        assert_true(r.status >= 500, "should fail on DB error"),
        # No event should appear — the DB write failed.
        assert_never(where=lambda e:
            e.type == "topic" and e.data.get("order_id") == 99),
    ],
)
```

This catches a common bug: publishing an event before confirming
the database write. If the DB fails after publish, the event is
orphaned — consumers process an order that doesn't exist.

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
def login_user():
    return api.http.post(path="/login", body='{"user":"alice"}')
scenario(login_user)

fault_scenario("session_created",
    scenario = login_user,
    faults = [],
    expect = lambda r: [
        assert_eq(r.status, 200),
        assert_eventually(where=lambda e:
            e.type == "poll" and e.service == "redis"
            and e.data.get("value") != ""),
    ],
)
```

## Combining event sources with syscall faults

The full power: fault at the syscall level, observe at the application
level. This tests the **end-to-end effect** of a low-level failure:

```python
def write_data():
    return api.post(path="/data/key1", body="value1")
scenario(write_data)

disk_failure = fault_assumption("disk_failure",
    target = db,
    write = deny("EIO", label="disk failure"),
)

fault_scenario("disk_fault_produces_error_log",
    scenario = write_data,
    faults = disk_failure,
    expect = lambda r:
        # Syscall fault caused the write to fail.
        # Did the service log something useful?
        assert_eventually(where=lambda e:
            e.type == "stdout" and e.service == "api"
            and "error" in e.data.get("msg", "").lower()),
)
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
- `fault_scenario()` with `expect=` handles both response assertions and trace assertions
- `assert_eventually` and `assert_never` work in `expect=` for event source verification
- Combine syscall faults + event sources to test end-to-end failure effects

## What's next

[Chapter 12: Named Operations](12-named-ops.md) — group related syscalls
into logical operations for cleaner fault specs.
