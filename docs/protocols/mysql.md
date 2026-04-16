# MySQL Protocol Reference

Interface declaration:

```python
db = service("mysql",
    interface("main", "mysql", 3306),
    image = "mysql:8",
    env = {"MYSQL_ROOT_PASSWORD": "test", "MYSQL_DATABASE": "testdb"},
    healthcheck = tcp("localhost:3306"),
)
```

## Methods

### `query(sql="", dsn="")`

Execute a SQL query that returns rows.

```python
resp = db.main.query(sql="SELECT * FROM users WHERE id=1")
# resp.data = [{"id": 1, "name": "alice"}]

resp = db.main.query(sql="SELECT count(*) as n FROM orders")
# resp.data = [{"n": 42}]
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sql` | string | required | SQL query |
| `dsn` | string | auto | MySQL DSN (auto-generated from service address) |

### `exec(sql="", dsn="")`

Execute a SQL statement that doesn't return rows.

```python
resp = db.main.exec(sql="INSERT INTO users (name) VALUES ('bob')")
# resp.data = {"rows_affected": 1}

resp = db.main.exec(sql="CREATE TABLE IF NOT EXISTS orders (id INT AUTO_INCREMENT PRIMARY KEY, item VARCHAR(255))")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sql` | string | required | SQL statement |
| `dsn` | string | auto | MySQL DSN |

## Response Object

Same as [Postgres](postgres.md#response-object) — `query()` returns list
of dicts, `exec()` returns `{"rows_affected": N}`.

## Fault Rules

### `error(query=, message=)`

Reject matching queries with a MySQL error.

```python
insert_fail = fault_assumption("insert_fail",
    target = db.main,
    rules = [error(query="INSERT*", message="disk full")],
)

read_only = fault_assumption("read_only",
    target = db.main,
    rules = [error(query="INSERT*", message="read only"),
             error(query="UPDATE*", message="read only"),
             error(query="DELETE*", message="read only")],
)
```

### `delay(query=, delay=)`

```python
slow_reads = fault_assumption("slow_reads",
    target = db.main,
    rules = [delay(query="SELECT*", delay="2s")],
)
```

### `drop(query=)`

```python
drop_writes = fault_assumption("drop_writes",
    target = db.main,
    rules = [drop(query="INSERT*")],
)
```

## Seed / Reset Patterns

```python
def seed_mysql():
    db.main.exec(sql="CREATE TABLE IF NOT EXISTS users (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(255))")
    db.main.exec(sql="CREATE TABLE IF NOT EXISTS orders (id INT AUTO_INCREMENT PRIMARY KEY, user_id INT, item VARCHAR(255))")
    db.main.exec(sql="INSERT INTO users (name) VALUES ('alice'), ('bob')")

def reset_mysql():
    db.main.exec(sql="SET FOREIGN_KEY_CHECKS=0")
    db.main.exec(sql="TRUNCATE TABLE orders")
    db.main.exec(sql="TRUNCATE TABLE users")
    db.main.exec(sql="SET FOREIGN_KEY_CHECKS=1")
    db.main.exec(sql="INSERT INTO users (name) VALUES ('alice'), ('bob')")

db = service("mysql",
    interface("main", "mysql", 3306),
    image = "mysql:8",
    env = {"MYSQL_ROOT_PASSWORD": "test", "MYSQL_DATABASE": "testdb"},
    healthcheck = tcp("localhost:3306"),
    reuse = True,
    seed = seed_mysql,
    reset = reset_mysql,
)
```

**Tip:** MySQL requires `SET FOREIGN_KEY_CHECKS=0` before truncating tables
with foreign key constraints.

## Event Sources

No native event source. Use `stdout` to capture MySQL logs, or `poll` for
periodic state checks:

```python
db = service("mysql", ...,
    observe = [stdout(decoder=logfmt_decoder())],
)
```
