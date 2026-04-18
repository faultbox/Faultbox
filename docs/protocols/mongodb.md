# MongoDB Protocol Reference

Interface declaration:

```python
db = service("mongo",
    interface("main", "mongodb", 27017),
    image = "mongo:7",
    env = {"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "test"},
    healthcheck = tcp("localhost:27017"),
)
```

Faultbox speaks the MongoDB Wire Protocol (OP_MSG, MongoDB 3.6+) — the same
format all modern drivers use. Step arguments accept Starlark dicts directly;
they are encoded as BSON on the wire.

## Methods

### `find(collection="", filter={}, limit=0, database="test")`

Query documents matching the filter.

```python
resp = db.main.find(collection="users", filter={"role": "admin"})
# resp.data = [{"_id": "6632...", "name": "alice", "role": "admin"}]

resp = db.main.find(collection="orders", filter={"status": "pending"}, limit=10)
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `collection` | string | required | Collection name |
| `filter` | dict | `{}` | MongoDB query filter (standard operators: `$gt`, `$in`, `$regex`, ...) |
| `limit` | int | 0 | Maximum documents to return (0 = no limit) |
| `database` | string | `"test"` | Database name |

### `insert(collection="", document={}, database="test")`

Insert one document.

```python
db.main.insert(collection="users", document={"name": "alice", "role": "admin"})
# resp.data = {"inserted_id": "6632..."}
```

### `insert_many(collection="", documents=[], database="test")`

Insert multiple documents.

```python
db.main.insert_many(collection="users", documents=[
    {"name": "alice", "role": "admin"},
    {"name": "bob", "role": "user"},
])
# resp.data = {"inserted_count": 2}
```

### `update(collection="", filter={}, update={}, database="test")`

Update all documents matching the filter.

```python
db.main.update(collection="users",
    filter={"role": "user"},
    update={"$set": {"active": True}},
)
# resp.data = {"matched": 5, "modified": 5}
```

### `delete(collection="", filter={}, database="test")`

Delete all documents matching the filter.

```python
db.main.delete(collection="orders", filter={"status": "cancelled"})
# resp.data = {"deleted": 12}
```

### `count(collection="", filter={}, database="test")`

Count documents matching the filter.

```python
resp = db.main.count(collection="orders", filter={"status": "pending"})
# resp.data = {"count": 42}
```

### `command(cmd={}, database="test")`

Run an arbitrary MongoDB command.

```python
db.main.command(cmd={"dropDatabase": 1})
db.main.command(cmd={"createIndexes": "users", "indexes": [{"key": {"email": 1}, "name": "email_1", "unique": True}]})

resp = db.main.command(cmd={"serverStatus": 1})
# resp.data = {"host": "...", "version": "7.0.0", ...}
```

## Response Object

| Field | Type | Description |
|-------|------|-------------|
| `.data` | dict/list | Operation result (see each method) |
| `.status` | int | 0 (success) |
| `.ok` | bool | `True` if operation succeeded |
| `.duration_ms` | int | Execution time |

ObjectIDs are stringified (hex representation). DateTime values are formatted
as RFC3339. All other BSON types pass through as their JSON equivalents.

## Fault Rules

### `error(collection=, op=, message=)`

Reject matching commands with a MongoDB server error. The error is encoded
as a valid BSON response with `ok=0`, so drivers raise their native
`WriteException` / `CommandError`.

```python
insert_fail = fault_assumption("insert_fail",
    target = db.main,
    rules = [error(collection="orders", op="insert", message="disk full")],
)

auth_fail = fault_assumption("auth_fail",
    target = db.main,
    rules = [error(op="saslStart", message="authentication failed")],
)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `collection` | string | Collection name glob pattern (also accepted as `key=`) |
| `op` | string | MongoDB command name: `find`, `insert`, `update`, `delete`, `aggregate`, etc. (also accepted as `method=`) |
| `message` | string | Error message surfaced to the driver |

### `delay(collection=, op=, delay=)`

Slow down matching commands.

```python
slow_reads = fault_assumption("slow_reads",
    target = db.main,
    rules = [delay(op="find", delay="3s")],
)

slow_orders = fault_assumption("slow_orders",
    target = db.main,
    rules = [delay(collection="orders", op="*", delay="2s")],
)
```

### `drop(collection=, op=)`

Close the connection when a matching command arrives. Drivers see a
connection reset and retry per their pool settings.

```python
drop_writes = fault_assumption("drop_writes",
    target = db.main,
    rules = [drop(collection="orders", op="insert")],
)
```

### Syscall-level faults

For broad infrastructure failures on the mongod process:

```python
disk_full = fault_assumption("disk_full",
    target = db,  # ServiceDef, not InterfaceRef
    write = deny("ENOSPC"),
)
```

## Recipes

Curated failures live in [recipes/mongodb.star](../../recipes/mongodb.star).
The file exports a single `mongodb` namespace struct whose fields are
one-line wrappers over the primitives above with the canonical MongoDB
error text baked in.

```python
load("./recipes/mongodb.star", "mongodb")

broken = fault_assumption("broken_mongo",
    target = db.main,
    rules  = [mongodb.disk_full(collection="orders")],
)

quorum_lost = fault_assumption("quorum_lost",
    target = db.main,
    rules  = [mongodb.replica_unavailable()],
)
```

Available recipes on `mongodb`: `disk_full`, `auth_failed`,
`replica_unavailable`, `slow_query`, `slow_writes`, `connection_drop`,
`duplicate_key_error`, `write_conflict`.

## Seed / Reset Patterns

```python
def seed_mongo():
    db.main.command(cmd={"dropDatabase": 1})
    db.main.insert_many(collection="users", documents=[
        {"name": "alice", "role": "admin"},
        {"name": "bob", "role": "user"},
    ])

def reset_mongo():
    db.main.command(cmd={"dropDatabase": 1})

db = service("mongo",
    interface("main", "mongodb", 27017),
    image = "mongo:7",
    env = {"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "test"},
    healthcheck = tcp("localhost:27017"),
    reuse = True,
    seed = seed_mongo,
    reset = reset_mongo,
)
```

## Notes

- The proxy parses OP_MSG to extract the command name and collection for
  rule matching. Older opcodes (OP_QUERY, OP_INSERT) are forwarded without
  inspection — modern drivers (3.6+) do not use them.
- SCRAM-SHA-256 authentication handshakes are passed through to the real
  mongod. Use `error(op="saslStart", ...)` to inject auth failures without
  a real auth backend.
- When using `reuse=True`, consider running `dropDatabase` in `reset=` to
  guarantee a clean state between tests.
