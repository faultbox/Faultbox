# Cassandra Protocol Reference

Interface declaration:

```python
cass = service("cassandra",
    interface("main", "cassandra", 9042),
    image = "cassandra:4.1",
    healthcheck = tcp("localhost:9042"),
)
```

Faultbox speaks the CQL Binary Protocol v4 — the version all modern
Cassandra drivers (Java, Python, Go, Node.js) use. The client side uses
the official `gocql` driver under the hood.

## Methods

### `query(cql="", consistency="ONE")`

Execute a CQL SELECT and return rows.

```python
resp = cass.main.query(cql="SELECT * FROM test.orders WHERE status='pending' ALLOW FILTERING")
# resp.data = [{"id": "uuid-...", "item": "widget", "status": "pending"}]

# Stronger consistency
resp = cass.main.query(cql="SELECT * FROM test.users", consistency="QUORUM")
```

### `exec(cql="", consistency="ONE")`

Execute a CQL DDL, INSERT, UPDATE, or DELETE.

```python
cass.main.exec(cql="CREATE KEYSPACE IF NOT EXISTS test WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}")
cass.main.exec(cql="INSERT INTO test.orders (id, item, status) VALUES (uuid(), 'widget', 'pending')")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cql` | string | required | CQL statement |
| `consistency` | string | `"ONE"` | Consistency level: ANY, ONE, TWO, THREE, QUORUM, ALL, LOCAL_QUORUM, EACH_QUORUM, LOCAL_ONE |

## Response Object

### For `query()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | list of dicts | Rows as `[{"col": value}, ...]` |
| `.ok` | bool | `True` on success |
| `.duration_ms` | int | Execution time |

UUIDs are stringified. Blobs (`[]byte`) pass as strings. Timestamps are
formatted as RFC3339 UTC.

### For `exec()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | dict | `{"ok": true}` |
| `.ok` | bool | `True` on success |

## Fault Rules

Fault rules match on CQL statement text. `EXECUTE` opcodes (prepared
statements reference a `prepared_id`, not CQL text) bypass rule matching
and forward unchanged — match via `query="*"` covers only raw QUERY.

### `error(query=, message=)`

Return a CQL server error.

```python
write_fail = fault_assumption("write_fail",
    target = cass.main,
    rules = [error(query="INSERT*", message="write timeout")],
)

unavailable = fault_assumption("unavailable",
    target = cass.main,
    rules = [error(query="*", message="Cannot achieve consistency level QUORUM")],
)
```

### `delay(query=, delay=)`

Slow matching statements.

```python
slow_reads = fault_assumption("slow_reads",
    target = cass.main,
    rules = [delay(query="SELECT*", delay="5s")],
)
```

### `drop(query=)`

Close the connection mid-statement.

```python
dropped_writes = fault_assumption("dropped_writes",
    target = cass.main,
    rules = [drop(query="INSERT*")],
)
```

## Recipes

See [recipes/cassandra.star](../../recipes/cassandra.star):

- `write_timeout`, `read_timeout` — coordinator timeouts
- `unavailable` — insufficient replicas for consistency level
- `overloaded` — node under load
- `slow_reads`, `slow_writes` — latency injection
- `connection_drop` — connection reset
- `schema_mismatch` — stale schema version

```python
load("./recipes/cassandra.star", "write_timeout", "unavailable")

broken = fault_assumption("quorum_lost",
    target = cass.main,
    rules  = [unavailable()],
)
```

## Seed / Reset Patterns

```python
def seed_cassandra():
    cass.main.exec(cql="CREATE KEYSPACE IF NOT EXISTS test WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}")
    cass.main.exec(cql="CREATE TABLE IF NOT EXISTS test.orders (id UUID PRIMARY KEY, item TEXT, status TEXT)")

def reset_cassandra():
    cass.main.exec(cql="TRUNCATE test.orders")

cass = service("cassandra",
    interface("main", "cassandra", 9042),
    image = "cassandra:4.1",
    healthcheck = tcp("localhost:9042"),
    reuse = True,
    seed = seed_cassandra,
    reset = reset_cassandra,
)
```

## Implementation notes

- Proxy parses CQL Binary Protocol v4 frame headers (9 bytes) and
  extracts CQL text from QUERY opcodes for rule matching.
- EXECUTE (prepared) and BATCH opcodes forward without CQL-level
  matching — reference a prepared_id rather than the CQL text itself.
  To match prepared statements, use `query="*"` or fault at the syscall
  level.
- Error frames use CQL error code 0x0000 (ServerError). Specific codes
  (UNAVAILABLE=0x1000, WRITE_TIMEOUT=0x1100) with their additional
  body fields are tracked as future work on [RFC-016](../rfcs/0016-new-protocols.md).
- Cassandra containers are multi-process (Java + zookeeper) — requires
  RFC-014 (Unix socket fd passing) for syscall-level faults.
