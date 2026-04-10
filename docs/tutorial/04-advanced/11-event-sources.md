# Chapter 9: Event Sources & Observability

**Duration:** 20 minutes
**Prerequisites:** [Chapter 4 (Traces & Assertions)](../02-syscall-level/04-traces.md) completed

## Goals & Purpose

So far, Faultbox observes your system through **syscall interception** —
it sees every `write`, `connect`, `fsync`. This is powerful but low-level.
You know a write happened, but not *what* was written. You know a connection
was made, but not *what query* was sent.

Real debugging needs higher-level visibility:
- What did the service log to stdout?
- What rows did Postgres insert?
- What messages appeared on the Kafka topic?
- What does the metrics endpoint show?

**Event sources** bridge this gap — they capture structured data from
external channels and emit it into the same trace as syscall events.
You query them with the same assertions and monitors.

This chapter teaches you to:
- **Capture service stdout** as structured trace events
- **Decode output** with JSON, logfmt, and regex decoders
- **Query custom events** with lambda predicates
- **Use `.data`** for auto-decoded structured access

## Capturing stdout

Add `observe=[stdout(...)]` to a service to capture its output:

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

def test_stdout_events():
    api.post(path="/data/key1", body="value1")
    # Query stdout events from db:
    logs = events(where=lambda e: e.type == "stdout" and e.service == "db")
    print("db logged", len(logs), "lines")
```

Each line of stdout is decoded by the decoder and emitted as a
`type="stdout"` event with `service="db"`.

## Decoders

Decoders parse raw output lines into structured fields. Three built-in:

### JSON decoder

```python
observe = [stdout(decoder=json_decoder())]
```

Parses each line as a JSON object. Fields become accessible on `.data`:

```python
# If db outputs: {"level":"INFO","msg":"SET key1 value1","op":"SET"}
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data["op"] == "SET")
```

### Logfmt decoder

```python
observe = [stdout(decoder=logfmt_decoder())]
```

Parses `key=value key2="quoted value"` format:

```python
# If db outputs: level=INFO msg="SET key1 value1" op=SET
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data["op"] == "SET")
```

### Regex decoder

```python
observe = [stdout(decoder=regex_decoder(
    pattern=r"WAL: (?P<action>\w+) (?P<path>.+)"
))]
```

Named capture groups become fields:

```python
# If db outputs: WAL: fsync /data/wal/000001
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data["action"] == "fsync")
```

## Auto-decoded `.data`

All event source events have a `.data` attribute that auto-decodes
JSON into native Starlark dicts and lists. You never call `json.decode()`:

```python
# e.data is already a dict — no decoding needed:
assert_eventually(where=lambda e:
    e.type == "stdout" and e.data["level"] == "ERROR")

# For nested JSON:
# {"user": {"id": 1, "name": "alice"}}
assert_eventually(where=lambda e:
    e.data.get("user", {}).get("name") == "alice")
```

Use `print()` to discover the structure:

```python
def test_inspect_events():
    api.post(path="/data/key1", body="value1")
    logs = events(where=lambda e: e.type == "stdout")
    for log in logs:
        print(log.data)  # shows the actual dict structure
```

## Using with assertions and monitors

Event source events are first-class — all assertion functions work:

```python
def test_db_logs_operations():
    """Verify db logs the SET operation on stdout."""
    api.post(path="/data/key1", body="value1")

    # assert_eventually with lambda:
    assert_eventually(where=lambda e:
        e.type == "stdout" and e.service == "db")

    # assert_never:
    assert_never(where=lambda e:
        e.type == "stdout" and e.data.get("level") == "ERROR")
```

Monitors can watch event source events in real-time:

```python
def test_no_errors_in_logs():
    """Monitor: fail if any service logs an ERROR."""
    monitor(lambda e:
        fail("error in logs: " + e.data.get("msg", ""))
            if e.data.get("level") == "ERROR",
        service="db",
    )

    api.post(path="/data/key1", body="value1")
    api.get(path="/data/key1")
```

## Protocol plugins

Beyond `http` and `tcp`, Faultbox includes protocol plugins for
popular databases and message brokers. Each protocol provides
step methods that return auto-decoded `.data`:

```python
# Postgres
db = service("pg",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test"},
)
resp = db.main.query(sql="SELECT * FROM users")
print(resp.data)  # [{"id": 1, "name": "alice"}]

# Redis
cache = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
)
cache.main.set(key="user:1", value="alice")
resp = cache.main.get(key="user:1")
print(resp.data["value"])  # "alice"
```

See the [Spec Language Reference — Protocols](../../spec-language.md#protocols)
for the full list (postgres, redis, mysql, kafka, nats, grpc).

## What you learned

- `observe=[stdout(decoder=json_decoder())]` captures service output
- Three decoders: `json_decoder()`, `logfmt_decoder()`, `regex_decoder()`
- Event source events are first-class — same assertions, monitors, ShiViz
- `.data` auto-decodes JSON — no `json.decode()` needed
- `print(e.data)` shows the structure for debugging
- Protocol plugins (postgres, redis, kafka, etc.) provide domain-specific step methods

## Exercises

1. **Log monitoring**: Add `observe=[stdout(decoder=json_decoder())]` to
   the api service. Write a test that asserts the api logs a specific
   message when processing a request.

2. **Error detection**: Write a monitor that fails the test if any service
   logs at ERROR level. Inject a fault and verify the monitor catches it.

3. **Structured queries**: If you have a Postgres container, use
   `db.main.query(sql="...")` and assert on `resp.data` fields.
