# Protocol Reference

Each protocol defines the methods available on service interface references,
the fault rules for protocol-level injection, and seed/reset patterns.

## Protocols

| Protocol | Interface | Methods | Fault Rules | Event Source |
|----------|-----------|---------|-------------|-------------|
| [HTTP](http.md) | `"http"` | get, post, put, delete, patch | response, error, delay, drop | — |
| [HTTP/2](http2.md) | `"http2"` | get, post, put, delete, patch | response, error, delay, drop | — |
| [TCP](tcp.md) | `"tcp"` | send | — (use syscall faults) | — |
| [Postgres](postgres.md) | `"postgres"` | query, exec | error, delay, drop | wal_stream |
| [MySQL](mysql.md) | `"mysql"` | query, exec | error, delay, drop | — |
| [Redis](redis.md) | `"redis"` | get, set, del, keys, ping, incr, lpush, rpush, lrange, command | error, delay, drop | — |
| [Kafka](kafka.md) | `"kafka"` | publish, consume | drop, delay, duplicate | topic |
| [NATS](nats.md) | `"nats"` | publish, request, subscribe | drop, delay | — |
| [gRPC](grpc.md) | `"grpc"` | call | error, delay, drop | — |
| [MongoDB](mongodb.md) | `"mongodb"` | find, insert, insert_many, update, delete, count, command | error, delay, drop | — |

## How to read protocol docs

Each protocol page has:

1. **Methods** — full signatures with parameters and examples
2. **Response Object** — what fields are available on the response
3. **Fault Rules** — what `response()`, `error()`, `delay()`, `drop()` accept
4. **Seed/Reset Patterns** — how to initialize and clean up this technology
5. **Event Sources** — what `observe=` produces (if applicable)
6. **Data Integrity Patterns** — how to verify state in `expect` lambdas

## Quick reference

### Using protocol methods

```python
# Declare the interface
db = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    ...
)

# Use protocol methods via interface reference
resp = db.main.query(sql="SELECT * FROM users")
resp = db.main.exec(sql="INSERT INTO users (name) VALUES ('alice')")
```

### Protocol-level faults

```python
# Target is the interface reference (db.main), not the service (db)
insert_fail = fault_assumption("insert_fail",
    target = db.main,
    rules = [error(query="INSERT*", message="disk full")],
)
```

### Syscall-level faults

```python
# Target is the service (db), not the interface
disk_full = fault_assumption("disk_full",
    target = db,
    write = deny("ENOSPC"),
)
```

Protocol-level faults target specific operations (one SQL query, one HTTP
path). Syscall-level faults target all I/O on the service. See
[Choosing Fault Levels](../guides/choosing-fault-levels.md) for when to
use each.
