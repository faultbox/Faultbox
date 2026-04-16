# Redis Protocol Reference

Interface declaration:

```python
cache = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
)
```

## Methods

### `get(key="")`

Get the value of a key.

```python
resp = cache.main.get(key="user:1")
# resp.data = {"value": "alice"}   (or {"value": None} if key doesn't exist)
```

### `set(key="", value="")`

Set a key-value pair.

```python
cache.main.set(key="user:1", value="alice")
cache.main.set(key="config:timeout", value="5000")
```

### `del(key="")`

Delete a key.

```python
cache.main.del(key="user:1")
```

### `keys(pattern="*")`

List keys matching a glob pattern.

```python
resp = cache.main.keys(pattern="user:*")
# resp.data = ["user:1", "user:2", "user:3"]

resp = cache.main.keys(pattern="*")
# resp.data = ["user:1", "config:timeout", ...]
```

### `ping()`

Check connectivity.

```python
resp = cache.main.ping()
# resp.data = {"value": "PONG"}
```

### `incr(key="")`

Increment an integer key.

```python
cache.main.set(key="counter", value="0")
cache.main.incr(key="counter")
resp = cache.main.get(key="counter")
# resp.data = {"value": "1"}
```

### `lpush(key="", value="")` / `rpush(key="", value="")`

Push to the left/right of a list.

```python
cache.main.lpush(key="queue", value="first")
cache.main.rpush(key="queue", value="last")
```

### `lrange(key="", start="0", stop="-1")`

Get a range of elements from a list.

```python
resp = cache.main.lrange(key="queue", start="0", stop="-1")
# resp.data = ["first", "last"]
```

### `command(cmd="", args=[])`

Execute any Redis command not covered by the methods above.

```python
cache.main.command(cmd="EXPIRE", args=["user:1", "3600"])
cache.main.command(cmd="FLUSHALL")
cache.main.command(cmd="HSET", args=["user:1", "name", "alice", "email", "alice@example.com"])
resp = cache.main.command(cmd="HGETALL", args=["user:1"])
```

## Response Object

| Field | Type | Description |
|-------|------|-------------|
| `.data` | varies | Parsed RESP value — string, list, int, or None |
| `.ok` | bool | `True` if no error |
| `.duration_ms` | int | Execution time |

## Fault Rules

### `error(command=, key=, message=)`

Return a Redis error for matching commands.

```python
set_fails = fault_assumption("set_fails",
    target = cache.main,
    rules = [error(command="SET", message="READONLY")],
)

key_error = fault_assumption("key_error",
    target = cache.main,
    rules = [error(key="order:*", message="WRONGTYPE")],
)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `command` | string | Redis command glob (`"SET"`, `"GET"`, `"*"`) |
| `key` | string | Key glob pattern (`"order:*"`, `"session:*"`) |
| `message` | string | Redis error string to return |

### `delay(command=, key=, delay=)`

```python
slow_reads = fault_assumption("slow_reads",
    target = cache.main,
    rules = [delay(command="GET", delay="500ms")],
)
```

### `drop(command=, key=)`

```python
drop_writes = fault_assumption("drop_writes",
    target = cache.main,
    rules = [drop(command="SET")],
)
```

## Seed / Reset Patterns

```python
def seed_redis():
    cache.main.command(cmd="FLUSHALL")
    cache.main.set(key="config:max_retries", value="3")
    cache.main.set(key="config:timeout_ms", value="5000")

cache = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
    reuse = True,
    seed = seed_redis,  # FLUSHALL + seed on every reset
)
```

**Tip:** For Redis, `seed` and `reset` are often the same function —
`FLUSHALL` clears everything, then you re-seed.

## Data Integrity Patterns

### No stale cache after failure

```python
fault_scenario("no_stale_cache",
    scenario = create_order,
    faults = redis_down,
    expect = lambda r: (
        assert_true(r.status >= 500),
        assert_eq(len(cache.main.keys(pattern="order:*").data), 0,
            "no cached order data after Redis failure"),
    ),
)
```

### Cache-DB consistency

```python
fault_scenario("cache_consistent",
    scenario = update_user,
    faults = slow_cache,
    expect = lambda r: (
        assert_eq(r.status, 200),
        # DB has the updated value
        assert_eq(db.main.query(sql="SELECT name FROM users WHERE id=1").data[0]["name"], "updated"),
        # Cache also has the updated value (or is empty)
        assert_true(
            cache.main.get(key="user:1").data.get("value") in [None, "updated"],
            "cache must be consistent or empty"),
    ),
)
```
