# Protocol Reference

Each protocol defines the methods available on service interface references,
the fault rules for protocol-level injection, and seed/reset patterns.

## Protocols

| Protocol | Interface | Methods | Fault Rules | Event Source |
|----------|-----------|---------|-------------|-------------|
| [HTTP](http.md) | `"http"` | get, post, put, delete, patch | response, error, delay, drop | — |
| [HTTP/2](http2.md) | `"http2"` | get, post, put, delete, patch | response, error, delay, drop | — |
| [TCP](tcp.md) | `"tcp"` | send | — (use syscall faults) | — |
| [UDP](udp.md) | `"udp"` | send, send_no_reply | drop, delay | — |
| [Postgres](postgres.md) | `"postgres"` | query, exec | error, delay, drop | wal_stream |
| [MySQL](mysql.md) | `"mysql"` | query, exec | error, delay, drop | — |
| [Redis](redis.md) | `"redis"` | get, set, del, keys, ping, incr, lpush, rpush, lrange, command | error, delay, drop | — |
| [Kafka](kafka.md) | `"kafka"` | publish, consume | drop, delay, duplicate | topic |
| [NATS](nats.md) | `"nats"` | publish, request, subscribe | drop, delay | — |
| [gRPC](grpc.md) | `"grpc"` | call | error, delay, drop | — |
| [MongoDB](mongodb.md) | `"mongodb"` | find, insert, insert_many, update, delete, count, command | error, delay, drop | — |
| [Cassandra](cassandra.md) | `"cassandra"` | query, exec | error, delay, drop | — |
| [ClickHouse](clickhouse.md) | `"clickhouse"` | query, exec | error, delay, drop | — |

## How to read protocol docs

Each protocol page has:

1. **Methods** — full signatures with parameters and examples
2. **Response Object** — what fields are available on the response
3. **Fault Rules** — what `response()`, `error()`, `delay()`, `drop()` accept
4. **Seed/Reset Patterns** — how to initialize and clean up this technology
5. **Event Sources** — what `observe=` produces (if applicable)
6. **Data Integrity Patterns** — how to verify state in `expect` lambdas

## Readiness: use `ready()`, not `tcp()` (v0.16.0)

```python
healthcheck = ready(timeout = "60s")     # ask the service
healthcheck = tcp("localhost:5432")      # ask the port
```

`tcp()` reports ready as soon as *something* is listening. For a container
that is true the instant Docker's port proxy binds — measured at **0 ms**
against a Postgres that needed ~10 seconds to serve its first query. Every
step issued in that window fails against a server that is merely still
starting.

`ready()` asks the service, through its interface's protocol plugin, using the
credentials the spec already declared. For Postgres and MySQL that is a real
`SELECT 1`; for Redis a `PING`; for MongoDB a `Ping`; for the rest, the
plugin's own check. It retries until the timeout, so a slow cold start is
waited out rather than guessed at.

The pages below still show `tcp()` in places where the port really is the
whole question (raw TCP, UDP). Everywhere a protocol has its own notion of
"serving", prefer `ready()`.

## Credentials come from the service (v0.16.0, extended v0.16.1)

You declare credentials once, in `env=`, using each image's own published
convention. Steps, healthchecks and `ready()` all pick them up; an explicit
kwarg on a step always wins.

| Protocol | Read from `env=` |
|---|---|
| Postgres | `POSTGRES_USER` (default `postgres`), `POSTGRES_PASSWORD`, `POSTGRES_DB` |
| MySQL | `MYSQL_USER` + `MYSQL_PASSWORD`, else `root` + `MYSQL_ROOT_PASSWORD`; `MYSQL_DATABASE` |
| MongoDB | `MONGO_INITDB_ROOT_USERNAME`, `MONGO_INITDB_ROOT_PASSWORD`, `MONGO_INITDB_DATABASE` |
| Redis | `REDIS_PASSWORD`, `REDIS_USER` |
| ClickHouse | `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`, `CLICKHOUSE_DB` |
| Cassandra | `CASSANDRA_USER`, `CASSANDRA_PASSWORD`, `CASSANDRA_KEYSPACE` |

```python
db = service("mysql",
    interface("main", "mysql", 3306),
    image = "mysql:8",
    env = {"MYSQL_ROOT_PASSWORD": "test", "MYSQL_DATABASE": "app"},
    healthcheck = ready(timeout = "120s"),
)

db.main.exec(sql = "INSERT INTO t VALUES (1)")   # authenticates as root/test on app
```

Protocols with no credential story yet — Kafka (SASL), NATS beyond
user/password, gRPC — accept them per step or not at all.

## Assert on step results

**Every step returns a result, and an unchecked result is not a test.**

```python
r = db.main.exec(sql = "INSERT INTO t VALUES (1)")
assert_true(r.ok, "insert failed: %s" % r.error)
```

This is not style advice. A protocol step that fails returns
`ok = False` — it does not raise, because a failing dependency is often
exactly what a fault-injection spec is provoking. So a spec that ignores
results passes whether or not anything worked.

Two real bugs reached release behind that gap. Postgres steps could not
authenticate against any password-protected server (fixed in v0.16.0), and
MySQL steps could not authenticate at all, nor select a database (fixed in
v0.16.1). Both were found the same way: by writing the first spec that
asserted on the result of a step.

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
