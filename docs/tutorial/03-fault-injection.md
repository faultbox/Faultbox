# Chapter 3: Fault Injection in Tests

In chapter 2 you tested happy paths. Now you'll break things on purpose
and verify your system handles failures correctly.

## Two-service topology

Build both services:
```bash
go build -o /tmp/mock-db ./poc/mock-db/
go build -o /tmp/mock-api ./poc/mock-api/
```

The mock-api is an HTTP service that stores data in mock-db via TCP.
Create `fault-test.star`:

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def test_happy_path():
    resp = api.post(path="/data/mykey", body="myvalue")
    assert_eq(resp.status, 200)

    resp = api.get(path="/data/mykey")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "myvalue")
```

Run it:
```bash
faultbox test fault-test.star
```

## The `fault()` builtin

`fault()` applies temporary faults to a service while running a callback:

```python
def test_db_write_failure():
    """DB writes fail with I/O error."""
    def scenario():
        resp = api.post(path="/data/failkey", body="value")
        assert_true(resp.status >= 500, "expected 5xx on DB write failure")
    fault(db, write=deny("EIO"), run=scenario)
```

When `fault()` runs:
1. Apply `write=deny("EIO")` to the db service's seccomp filter
2. Run `scenario()`
3. Remove the fault (even if scenario fails)

The fault is **scoped** — it only exists during the callback. After `fault()`
returns, the db service works normally again.

## deny() and delay()

Two fault types:

```python
deny("EIO")                    # fail with I/O error, 100% probability
deny("ENOSPC")                 # fail with disk full
deny("ECONNREFUSED")           # fail with connection refused
deny("EIO", probability="50%") # fail 50% of the time

delay("500ms")                 # slow down by 500ms, then allow
delay("2s", probability="30%") # 30% chance of 2s delay
```

## Syscall keywords

The keyword in `fault()` is the syscall name:

```python
fault(db, write=deny("EIO"), run=fn)      # deny write() syscalls
fault(db, connect=deny("ECONNREFUSED"), run=fn) # deny connect()
fault(db, openat=deny("ENOENT"), run=fn)  # deny file opens
fault(db, fsync=deny("EIO"), run=fn)      # deny fsync
fault(db, sendto=deny("EPIPE"), run=fn)   # deny network sends
```

**Syscall families**: `write` automatically covers `write`, `writev`, and
`pwrite64`. You don't need to know which variant your program uses.

| Keyword | Covers |
|---------|--------|
| `write` | write, writev, pwrite64 |
| `read` | read, readv, pread64 |
| `fsync` | fsync, fdatasync |
| `sendto` | sendto, sendmsg |
| `recvfrom` | recvfrom, recvmsg |
| `open` | open, openat |

## Multi-fault injection

Apply multiple faults at once:

```python
def test_everything_broken():
    def scenario():
        resp = api.post(path="/data/key", body="val")
        assert_true(resp.status >= 500)
    fault(db,
        write=delay("1s"),
        fsync=deny("EIO"),
        run=scenario,
    )
```

## Imperative fault control

For more control, use `fault_start()` / `fault_stop()`:

```python
def test_imperative():
    # Start with no faults.
    resp = api.post(path="/data/key1", body="before")
    assert_eq(resp.status, 200)

    # Enable fault.
    fault_start(db, write=deny("EIO"))

    resp = api.post(path="/data/key2", body="during")
    assert_true(resp.status >= 500)

    # Disable fault.
    fault_stop(db)

    resp = api.post(path="/data/key3", body="after")
    assert_eq(resp.status, 200)
```

Prefer `fault()` with `run=` when possible — it guarantees cleanup.

## Diagnostic output

When a fault fires, the trace shows it:

```
--- PASS: test_db_write_failure (200ms, seed=0) ---
  syscall trace (85 events):
    #72  db    write   deny(input/output error)
    #73  db    write   deny(input/output error)
  fault applied to db: write=deny(EIO) -> filter:[write,writev,pwrite64]
```

If a fault was applied but never fired, you'll see a warning:

```
  fault applied to db: fsync=deny(EIO) -> filter:[fsync,fdatasync]
  per-service syscall summary:
    db    120 total, 0 faulted  [write:45 read:30 connect:5 ...]
```

This tells you the program might use a different syscall than expected.

## What you learned

- `fault(service, syscall=deny/delay, run=fn)` injects scoped faults
- `deny("ERRNO")` fails syscalls with a specific error code
- `delay("duration")` slows syscalls before allowing them
- Syscall families expand automatically (write covers pwrite64, writev)
- Diagnostic output shows which faults fired and which didn't
- `fault_start()` / `fault_stop()` for imperative control

## Exercises

1. **Slow database**: Write a test where DB writes take 500ms.
   Assert that the API response takes more than 400ms:
   ```python
   assert_true(resp.duration_ms > 400, "expected slow response")
   ```

2. **Connection failure**: Write a test where the API can't connect to
   the DB (`fault(api, connect=deny("ECONNREFUSED"), ...)`).
   Verify the API returns a 500 error.

3. **Disk full**: Write a test with `write=deny("ENOSPC")` on the DB.
   What error message does the API return in `resp.body`?

4. **Partial failure**: Use `deny("EIO", probability="50%")` on DB writes.
   Run the test 10 times (`--runs 10`). How many pass vs fail?
