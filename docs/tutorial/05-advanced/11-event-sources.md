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
- **Prove data reached Postgres** via the service's own commit logs
- **Verify Kafka messages were published** from publish logs
- **Confirm Redis state changes** from session-write logs
- **Query events with lambdas** for precise assertions

> **Which sources are built in?** `observe.stdout` and `observe.stderr`
> are part of the Starlark runtime and work in every spec. Dedicated
> Postgres WAL, Kafka topic, and Redis poll sources are Go event-source
> plugins - not builtins you call from a spec. The Postgres/Kafka/Redis
> sections below therefore use the stdout source: the service logs its
> own effects, and you assert on those log events.

## Stdout: capturing service logs

The simplest event source, and the one built into the Starlark runtime.
Add `observe=[observe.stdout(...)]` to capture a service's output:

```python
BIN = "bin/linux"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
    observe = [observe.stdout(decoder=decoder("json"))],
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
observe = [observe.stdout(decoder=decoder("json"))]

# In an expect lambda or assertion:
# If service outputs: {"level":"INFO","msg":"SET key1 value1","op":"SET"}
# assert_eventually(where=lambda e:
#     e.type == "stdout" and e.data.get("op") == "SET")
```

### Logfmt decoder

For services using logfmt (`key=value` pairs):

```python
observe = [observe.stdout(decoder=decoder("logfmt"))]

# If service outputs: level=INFO msg="SET key1 value1" op=SET
# assert_eventually(where=lambda e:
#     e.type == "stdout" and e.data.get("op") == "SET")
```

### Regex decoder

For services with custom log formats:

```python
observe = [observe.stdout(decoder=decoder("regex",
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

## Postgres: proving data reached the database

> **Requires Docker.** See [Chapter 9](09-containers.md) for container setup.

The `observe.stdout` / `observe.stderr` sources are the ones built into
the Starlark runtime. Richer external channels - Postgres WAL streaming,
Kafka topic consumers, Redis pollers - are provided by **event-source
plugins** (Go, registered at compile time) and are not Starlark builtins
you call from a spec. Calling `wal_stream(...)`, `topic(...)`, or
`poll(...)` in a spec that hasn't loaded the corresponding plugin fails
with `undefined`.

The pattern below works with the built-in stdout source: have the
service log its database effects as structured JSON, then assert on
those log events. This proves the write actually reached Postgres - not
just that the API returned 201.

```python
pg = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "mydb"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api",
    interface("http", "http", 8080),
    image = "myapp:latest",
    env = {"DATABASE_URL": "postgres://test@" + pg.main.internal_addr + "/mydb"},
    depends_on = [pg],
    healthcheck = http("localhost:8080/health"),
    # The API logs each committed row as JSON, e.g.
    # {"event":"row_inserted","table":"orders"}
    observe = [observe.stdout(decoder=decoder("json"))],
)
```

```python
def create_order():
    return api.http.post(path="/orders", body='{"item":"widget","qty":1}')
scenario(create_order)

fault_scenario("order_persisted_to_db",
    scenario = create_order,
    faults = [],
    expect = lambda r: [
        assert_eq(r.status, 201),
        # The log event proves the row was inserted - not just that
        # the API returned 201. It actually reached the database.
        assert_eventually(where=lambda e:
            e.type == "stdout" and e.service == "api"
            and e.data.get("event") == "row_inserted"
            and e.data.get("table") == "orders"),
    ],
)
```

### Verify rollback

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
            e.type == "stdout" and e.service == "api"
            and e.data.get("event") == "row_inserted"
            and e.data.get("table") == "orders"),
    ],
)
```

**Why this matters:** the API returned an error, but did it actually
rollback the database transaction? `assert_never` on the commit log proves
no data was persisted — the strongest guarantee you can make.

## Kafka: verifying published messages

> **Requires Docker.**

Watching a Kafka topic directly needs the `topic` event-source plugin.
Without it, the loadable pattern is the same as for Postgres: have the
worker log each message it publishes as structured JSON, then assert on
those log events.

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    healthcheck = tcp("localhost:9092"),
)

worker = service("worker",
    interface("http", "http", 8080),
    image = "myapp-worker:latest",
    env = {"KAFKA_BROKERS": kafka.broker.internal_addr},
    depends_on = [kafka],
    healthcheck = http("localhost:8080/health"),
    # The worker logs each published message, e.g.
    # {"event":"published","topic":"order-events","order_id":42}
    observe = [observe.stdout(decoder=decoder("json"))],
)
```

The worker logs each message it publishes; we assert those log events
carry the expected payload.

```python
def process_order():
    return worker.http.post(path="/process", body='{"order_id": 42}')
scenario(process_order)

fault_scenario("order_event_published",
    scenario = process_order,
    faults = [],
    expect = lambda r: [
        assert_eq(r.status, 200),
        # The publish should be logged.
        assert_eventually(where=lambda e:
            e.type == "stdout" and e.service == "worker"
            and e.data.get("event") == "published"
            and e.data.get("order_id") == 42),
    ],
)
```

### Message loss detection

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
        # No publish should be logged - the DB write failed.
        assert_never(where=lambda e:
            e.type == "stdout" and e.service == "worker"
            and e.data.get("event") == "published"
            and e.data.get("order_id") == 99),
    ],
)
```

This catches a common bug: publishing an event before confirming
the database write. If the DB fails after publish, the event is
orphaned — consumers process an order that doesn't exist.

> **Note on `.data` types.** `.data` auto-decodes the JSON log line into
> native Starlark values: a JSON string stays a string, a JSON number
> becomes an int (so `order_id` of 42 compares as `42`, not `"42"`).
> Match against the native type in your predicates.

## Redis: verifying state changes

> **Requires Docker.**

Polling Redis directly needs the `poll` event-source plugin. With the
built-in stdout source, have the API log the session it writes to Redis
and assert on that:

```python
redis = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
)

api = service("api",
    interface("http", "http", 8080),
    image = "myapp:latest",
    env = {"REDIS_ADDR": redis.main.internal_addr},
    depends_on = [redis],
    healthcheck = http("localhost:8080/health"),
    # The API logs session writes, e.g.
    # {"event":"session_created","user":"alice"}
    observe = [observe.stdout(decoder=decoder("json"))],
)
```

The API logs each session it creates; we assert that log event appears:

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
            e.type == "stdout" and e.service == "api"
            and e.data.get("event") == "session_created"),
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

**JSON** - all events (syscall + stdout events) in one file
with vector clocks, timestamps, and service attribution.

**ShiViz** — event source events appear on the same service swimlane as
syscall events. Open at https://bestchai.bitbucket.io/shiviz/ to see:
- DB syscall events AND stdout log events on the same timeline
- Causal arrows between services
- The exact ordering: "the write syscall happened, then the service
  logged the committed row, then the downstream call was made"

This is useful for debugging distributed transactions — did the event
publish happen before or after the database commit?

## What you learned

- `observe=[observe.stdout(decoder=...)]` captures service output as events
- Three decoders: `decoder("json")`, `decoder("logfmt")`, `decoder("regex", pattern=...)`
- `.data` auto-decodes - no `json.decode()` needed (fields arrive as strings)
- `observe.stdout` / `observe.stderr` are the runtime's built-in event
  sources; WAL / Kafka / Redis channels need Go event-source plugins
- Have services log their DB commits, publishes, and state changes as
  JSON - then assert on those `stdout` events for end-to-end guarantees
- `fault_scenario()` with `expect=` handles both response assertions and trace assertions
- `assert_eventually` and `assert_never` work in `expect=` for event source verification
- Combine syscall faults + event sources to test end-to-end failure effects

## What's next

[Chapter 12: Named Operations](12-named-ops.md) — group related syscalls
into logical operations for cleaner fault specs.
