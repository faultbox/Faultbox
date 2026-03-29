# Chapter 3: Fault Injection in Tests

**Duration:** 25 minutes
**Platform:** Linux native, or macOS via Lima VM

## Goals & Purpose

In chapter 2 you wrote tests that verify happy paths. But happy-path tests
only prove your system works when nothing goes wrong — which is the easy case.

The hard question is: **"What happens when component X fails in way Y?"**

For every dependency your service has (database, cache, filesystem, network),
there are failure modes: timeouts, I/O errors, disk full, connection refused,
partial writes. Most codebases have error handling for these — but is it
*correct*? Does the API return 503 or does it return corrupted data? Does
it retry or does it hang?

This chapter teaches you to:
- **Think in failure modes** — for each dependency, enumerate what can go wrong
- **Write targeted fault scenarios** — "when the DB can't write, the API should return 503"
- **Use scoped faults** — break one thing, verify the rest still works
- **Read diagnostic output** — when faults don't fire, understand why

After this chapter, your mental checklist for any new feature will include:
"what are the failure modes, and have I tested each one?"

## Build the two-service system

**Linux and macOS (same):**
```bash
go build -o /tmp/mock-db ./poc/mock-db/
go build -o /tmp/mock-api ./poc/mock-api/
```

The mock-api is an HTTP service that stores data in mock-db via TCP.
Two services, one dependency — the simplest distributed system.

## Topology with dependencies

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

Notice: `depends_on = [db]` tells Faultbox to start db before api.
`env = {"DB_ADDR": db.main.addr}` wires the api to the db's address.

**The pattern:** topology is declared, not discovered. You're writing down
"the api depends on the db at this address" — making the architecture explicit.

## The `fault()` builtin

`fault()` applies temporary faults while running a callback:

```python
def test_db_write_failure():
    """DB writes fail with I/O error — API should return 500."""
    def scenario():
        resp = api.post(path="/data/failkey", body="value")
        assert_true(resp.status >= 500, "expected 5xx on DB write failure")
    fault(db, write=deny("EIO"), run=scenario)
```

What happens:
1. **Apply** `write=deny("EIO")` to the db service's seccomp filter
2. **Run** `scenario()` — which exercises the fault
3. **Remove** the fault (even if scenario fails)

The fault is **scoped** — it exists only during the callback. This is
important: you're testing a specific hypothesis ("when db writes fail,
the api returns 500"), not creating permanent damage.

## deny() and delay()

Two ways to break things:

```python
# Fail immediately with an error code
deny("EIO")                    # I/O error, 100%
deny("ENOSPC")                 # disk full
deny("ECONNREFUSED")           # connection refused
deny("EIO", probability="50%") # 50% chance

# Slow down, then allow
delay("500ms")                 # add 500ms latency
delay("2s", probability="30%") # 30% chance of 2s delay
```

**When to use deny vs delay:**
- `deny` tests error handling — does your code catch the error and respond correctly?
- `delay` tests timeout handling — does your code time out gracefully or hang forever?

Slow failures are often worse than fast failures. A 4.9s delay with a 5s
timeout looks like success but cascades into downstream timeouts.

## Syscall keywords

The keyword in `fault()` is the syscall name:

```python
fault(db, write=deny("EIO"), run=fn)      # deny write() syscalls
fault(api, connect=deny("ECONNREFUSED"), run=fn) # deny connect()
fault(db, openat=deny("ENOENT"), run=fn)  # deny file opens
fault(db, fsync=deny("EIO"), run=fn)      # deny fsync
```

**Syscall families** expand automatically — you don't need to know which
low-level variant your program uses:

| You write | Actually covers | Why |
|-----------|-----------------|-----|
| `write=deny(...)` | write, writev, pwrite64 | Postgres uses pwrite64 for data pages |
| `read=deny(...)` | read, readv, pread64 | Java uses readv for NIO |
| `fsync=deny(...)` | fsync, fdatasync | Postgres uses fdatasync for WAL |
| `sendto=deny(...)` | sendto, sendmsg | Nginx uses sendmsg |
| `open=deny(...)` | open, openat | Modern Linux uses openat exclusively |

**The intuition:** think in terms of operations (write, read, sync), not
syscall numbers. Faultbox handles the mapping.

## Multi-fault injection

Apply multiple faults at once to simulate cascading failures:

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

For scenarios where fault timing matters:

```python
def test_fault_mid_operation():
    # Phase 1: no faults.
    resp = api.post(path="/data/key1", body="before")
    assert_eq(resp.status, 200)

    # Phase 2: enable fault.
    fault_start(db, write=deny("EIO"))
    resp = api.post(path="/data/key2", body="during")
    assert_true(resp.status >= 500)

    # Phase 3: disable fault.
    fault_stop(db)
    resp = api.post(path="/data/key3", body="after")
    assert_eq(resp.status, 200)
```

Prefer `fault()` with `run=` when possible — it guarantees cleanup.

## Reading diagnostic output

When a fault fires:
```
--- PASS: test_db_write_failure (200ms) ---
  syscall trace (85 events):
    #72  db    write   deny(input/output error)
    #73  db    write   deny(input/output error)
  fault applied to db: write=deny(EIO) -> filter:[write,writev,pwrite64]
```

When a fault was applied but never fired:
```
  fault applied to db: fsync=deny(EIO) -> filter:[fsync,fdatasync]
  per-service syscall summary:
    db    120 total, 0 faulted  [write:45 read:30 connect:5 ...]
```

**If faults don't fire:** look at the syscall summary. If the program uses
`pwrite64` but your filter only has `write`, the expansion might be missing.
The diagnostic tells you exactly which syscalls the service actually used.

## What you learned

- `fault(service, syscall=deny/delay, run=fn)` injects scoped faults
- `deny()` tests error handling, `delay()` tests timeout handling
- Syscall families expand automatically (write covers pwrite64, writev)
- Multi-fault injection simulates cascading failures
- Diagnostic output shows which faults fired and which didn't

**The mental checklist:** for every dependency, ask:
1. What if writes fail? (`write=deny("EIO")`)
2. What if the connection drops? (`connect=deny("ECONNREFUSED")`)
3. What if it's slow? (`write=delay("2s")`)
4. What if the disk is full? (`write=deny("ENOSPC")`)
5. What if sync fails? (`fsync=deny("EIO")`)

## What's next

You can now break things and verify the response. But how do you know what
*actually happened* inside the system? When 85 syscalls are intercepted,
which ones mattered?

Chapter 4 introduces the event trace — a complete record of every syscall
the system made — and temporal assertions that let you reason about ordering:
"did the WAL write happen before the response?" "did the retry happen?"

## Exercises

1. **Slow database**: Write a test where DB writes take 500ms. Assert the
   API response is slow:
   ```python
   assert_true(resp.duration_ms > 400, "expected slow response")
   ```

2. **Connection failure**: Write a test where the API can't connect to the DB.
   Verify the API returns 500 with a meaningful error in `resp.body`.

3. **Disk full**: Write a test with `write=deny("ENOSPC")` on the DB.
   What's in `resp.body`? Is the error message useful to an operator?

4. **Partial failure**: Use `deny("EIO", probability="50%")` on DB writes.
   Run with `--runs 10`. How many pass? How many fail? What does this
   tell you about your error handling's consistency?
