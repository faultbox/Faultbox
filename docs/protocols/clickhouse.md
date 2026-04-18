# ClickHouse Protocol Reference

Interface declaration:

```python
ch = service("clickhouse",
    interface("main", "clickhouse", 8123),
    image = "clickhouse/clickhouse-server:24",
    healthcheck = tcp("localhost:8123"),
)
```

Faultbox uses ClickHouse's **HTTP interface** (default port 8123), not the
native binary protocol on 9000. This is the simplification flagged in
RFC-016 — HTTP is significantly easier to proxy and covers the majority
of production use cases. For workloads that specifically need the native
protocol, use syscall-level faults on the service.

## Methods

### `query(sql="")`

Execute a SELECT and return rows. The plugin automatically appends
`FORMAT JSON` so responses parse structurally.

```python
resp = ch.main.query(sql="SELECT count() as n FROM events WHERE date = today()")
# resp.data = [{"n": 1000000}]

# User-specified formats are respected (no FORMAT JSON appended).
resp = ch.main.query(sql="SELECT * FROM events FORMAT CSV")
```

### `exec(sql="")`

Execute DDL, INSERT, or any statement that doesn't need structured output.

```python
ch.main.exec(sql="CREATE TABLE IF NOT EXISTS events (date Date, type String) ENGINE = MergeTree() ORDER BY date")
ch.main.exec(sql="INSERT INTO events (date, type) VALUES (today(), 'order')")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sql` | string | required | SQL statement |

## Response Object

### For `query()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | list of dicts | Rows as `[{"col": value}, ...]` |
| `.fields["rows"]` | string | Row count reported by ClickHouse |
| `.ok` | bool | `True` on HTTP 200 |
| `.duration_ms` | int | Execution time |

### For `exec()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | dict | `{"ok": true}` |
| `.ok` | bool | `True` on HTTP 200 |

On non-200 responses, `.ok` is `False` and `.data.error` contains the raw
ClickHouse exception text.

## Fault Rules

Fault rules match on SQL text. The proxy extracts SQL from either the
POST body or the `?query=...` URL parameter — both driver conventions.

### `error(query=, message=)`

Return a ClickHouse-shaped exception (HTTP 500 with
`Code: N. DB::Exception: <message>` body).

```python
overload = fault_assumption("overload",
    target = ch.main,
    rules = [error(query="INSERT*", message="Too many parts")],
)
```

### `delay(query=, delay=)`

Slow matching queries.

```python
slow_reports = fault_assumption("slow_reports",
    target = ch.main,
    rules = [delay(query="SELECT*", delay="5s")],
)
```

### `drop(query=)`

Close the HTTP connection mid-request.

```python
flaky = fault_assumption("flaky",
    target = ch.main,
    rules = [drop(query="INSERT*", probability="10%")],
)
```

## Recipes

See [recipes/clickhouse.star](../../recipes/clickhouse.star):

- `too_many_parts` — insert rate exceeds merge rate
- `memory_limit` — query exceeds memory quota
- `table_not_exists` — missing table
- `readonly_mode` — writes rejected during maintenance
- `slow_analytics`, `slow_ingest` — latency injection
- `connection_drop` — connection reset mid-query
- `replica_stale` — replica too far behind leader

```python
load("@faultbox/recipes/clickhouse.star", "clickhouse")

broken = fault_assumption("overloaded",
    target = ch.main,
    rules  = [clickhouse.too_many_parts(), clickhouse.memory_limit(query="SELECT * FROM huge_table*")],
)
```

## Seed / Reset Patterns

```python
def seed_clickhouse():
    ch.main.exec(sql="CREATE TABLE IF NOT EXISTS events (date Date, type String, data String) ENGINE = MergeTree() ORDER BY date")

def reset_clickhouse():
    ch.main.exec(sql="TRUNCATE TABLE events")

ch = service("clickhouse",
    interface("main", "clickhouse", 8123),
    image = "clickhouse/clickhouse-server:24",
    healthcheck = tcp("localhost:8123"),
    reuse = True,
    seed = seed_clickhouse,
    reset = reset_clickhouse,
)
```

## Implementation notes

- Client POSTs SQL as the HTTP body with `default_format=JSON`. For
  SELECT queries, the plugin appends `FORMAT JSON` unless the user
  specified a format.
- Proxy reads the request body into memory (1 MiB cap) to match against
  the `query=` pattern, then replaces the body so the reverse proxy can
  forward it.
- Error injection uses `Code: 0. DB::Exception: <message>` — the
  generic shape. Specific ClickHouse error codes (60 for missing table,
  241 for memory limit) are not currently emitted; drivers typically
  match on the exception text.
