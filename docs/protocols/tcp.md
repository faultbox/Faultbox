# TCP Protocol Reference

Interface declaration:

```python
db = service("db",
    interface("main", "tcp", 5432),
    ...
)
```

## Methods

### `send(data="")`

Send a line of data over TCP and read the response.

```python
resp = db.main.send(data="PING")
assert_eq(resp, "PONG")

resp = db.main.send(data="SET mykey myvalue")
resp = db.main.send(data="GET mykey")
assert_eq(resp, "myvalue")
```

**Note:** `send()` returns the first line of the response as a string
(not a response object). This is a low-level protocol — for structured
database access, use the `postgres`, `mysql`, or `redis` protocols instead.

## Response

`send()` returns a **string** — the first line received from the server.

```python
result = db.main.send(data="PING")
# result is "PONG" (string, not response object)
```

## Fault Rules

TCP has no protocol-level fault rules — it operates at the byte level
with no request/response structure. Use syscall-level faults:

```python
db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
)

db_slow = fault_assumption("db_slow",
    target = db,
    write = delay("500ms"),
)
```

## Seed / Reset Patterns

For TCP services with simple text protocols:

```python
def seed_db():
    db.main.send(data="SET user:1 alice")
    db.main.send(data="SET user:2 bob")

def reset_db():
    db.main.send(data="FLUSHALL")

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    reuse = True,
    seed = seed_db,
    reset = reset_db,
)
```
