# Postgres Protocol Reference

Interface declaration:

```python
db = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432"),
)
```

## Methods

### `query(sql="", connstr="")`

Execute a SQL query that returns rows (SELECT, RETURNING).

```python
resp = db.main.query(sql="SELECT * FROM users WHERE id=1")
# resp.data = [{"id": 1, "name": "alice", "email": "alice@example.com"}]

resp = db.main.query(sql="SELECT count(*) as n FROM orders")
# resp.data = [{"n": 42}]

# Custom connection string (overrides default)
resp = db.main.query(sql="SELECT 1", connstr="postgres://user:pass@host/db")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sql` | string | required | SQL query to execute |
| `connstr` | string | auto | Connection string (auto-generated from service address) |

### `exec(sql="", connstr="")`

Execute a SQL statement that doesn't return rows (INSERT, UPDATE, DELETE, DDL).

```python
resp = db.main.exec(sql="INSERT INTO users (name) VALUES ('bob')")
# resp.data = {"rows_affected": 1}

resp = db.main.exec(sql="CREATE TABLE IF NOT EXISTS orders (id SERIAL, item TEXT)")
# resp.data = {"rows_affected": 0}

resp = db.main.exec(sql="DELETE FROM orders WHERE status='cancelled'")
# resp.data = {"rows_affected": 5}
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sql` | string | required | SQL statement to execute |
| `connstr` | string | auto | Connection string |

## Response Object

### For `query()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | list of dicts | Rows as `[{"col": value}, ...]` |
| `.status` | int | 0 (success) |
| `.ok` | bool | `True` if query succeeded |
| `.duration_ms` | int | Execution time |

### For `exec()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data` | dict | `{"rows_affected": N}` |
| `.status` | int | 0 (success) |
| `.ok` | bool | `True` if statement succeeded |
| `.duration_ms` | int | Execution time |

## Fault Rules

### `error(query=, message=)`

Reject matching queries with a Postgres error.

```python
insert_fail = fault_assumption("insert_fail",
    target = db.main,
    rules = [error(query="INSERT*", message="disk full")],
)

select_fail = fault_assumption("select_fail",
    target = db.main,
    rules = [error(query="SELECT * FROM orders*", message="relation does not exist")],
)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `query` | string | SQL query glob pattern |
| `message` | string | Postgres error message to return |

### `delay(query=, delay=)`

Delay matching queries.

```python
slow_queries = fault_assumption("slow_queries",
    target = db.main,
    rules = [delay(query="SELECT*", delay="3s")],
)
```

### `drop(query=)`

Close connection when matching query arrives.

```python
drop_inserts = fault_assumption("drop_inserts",
    target = db.main,
    rules = [drop(query="INSERT*")],
)
```

### Syscall-level faults

For broad infrastructure failures:

```python
disk_full = fault_assumption("disk_full",
    target = db,  # ServiceDef, not InterfaceRef
    write = deny("ENOSPC"),
)

disk_error = fault_assumption("disk_error",
    target = db,
    write = deny("EIO"),
)
```

## Seed / Reset Patterns

### Schema + seed data

```python
def seed_postgres():
    db.main.exec(sql="CREATE TABLE IF NOT EXISTS users (id SERIAL PRIMARY KEY, name TEXT)")
    db.main.exec(sql="CREATE TABLE IF NOT EXISTS orders (id SERIAL PRIMARY KEY, user_id INT, item TEXT, status TEXT)")
    db.main.exec(sql="INSERT INTO users (name) VALUES ('alice'), ('bob')")

def reset_postgres():
    db.main.exec(sql="TRUNCATE users, orders RESTART IDENTITY CASCADE")

db = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432"),
    reuse = True,
    seed = seed_postgres,
    reset = reset_postgres,
)
```

### Heavy migrations + light reset

```python
def seed_full():
    """Run migrations + large seed. Slow (~5s)."""
    db.main.exec(sql=open("./migrations.sql").read())
    db.main.exec(sql=open("./seed.sql").read())

def reset_fast():
    """Truncate data only. Fast (~100ms)."""
    db.main.exec(sql="TRUNCATE orders, payments, inventory RESTART IDENTITY CASCADE")

db = service("postgres", ...,
    reuse = True,
    seed = seed_full,
    reset = reset_fast,
)
```

## Event Sources

### WAL Stream

Captures Postgres Write-Ahead Log changes in real-time:

```python
db = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test"},
    healthcheck = tcp("localhost:5432"),
    observe = [wal_stream(tables=["orders", "users"])],
)
```

WAL events have type `"wal"` with fields:

| Field | Type | Description |
|-------|------|-------------|
| `op` | string | `INSERT`, `UPDATE`, `DELETE`, `BEGIN`, `COMMIT` |
| `table` | string | Table name |
| `data` | dict | Row data (auto-decoded) |

```python
# Verify a row was inserted
assert_eventually(where=lambda e:
    e.type == "wal" and e.data.get("table") == "orders"
    and e.data.get("op") == "INSERT")

# Verify no writes during fault
assert_never(where=lambda e:
    e.type == "wal" and e.data.get("op") == "INSERT")
```

## Data Integrity Patterns

### No partial rows after error

```python
fault_scenario("no_orphan_orders",
    scenario = create_order,
    faults = disk_error,
    expect = lambda r: (
        assert_true(r.status >= 500),
        assert_eq(
            db.main.query(sql="SELECT count(*) as n FROM orders WHERE status='pending'").data[0]["n"],
            0,
            "no orphaned rows"),
    ),
)
```

### Verify INSERT was rejected

```python
fault_scenario("insert_rejected",
    scenario = create_order,
    faults = insert_fail,
    expect = lambda r: (
        assert_true(r.status >= 500),
        assert_eventually(type="proxy", where=lambda e:
            "INSERT" in e.data.get("query", "") and e.data.get("action") == "error"),
    ),
)
```
