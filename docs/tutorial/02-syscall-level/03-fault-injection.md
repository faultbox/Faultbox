# Chapter 3: Fault Injection in Tests

**Duration:** 25 minutes
**Prerequisites:** [Chapter 0 (Setup)](../00-prelude/00-setup.md) completed

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

## The two-service system

Both binaries were built in Chapter 0 (`make build` or `make demo-build`).

The mock-api is an HTTP service that stores data in mock-db via TCP.
Two services, one dependency — the simplest distributed system.

## Topology with dependencies

Create `fault-test.star` in the project root:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", BIN + "/mock-api",
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

**Linux:**
```bash
bin/faultbox test fault-test.star
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox test fault-test.star
```

Expected output:
```
... [starlark] running test  test=test_happy_path
... [starlark] starting session  binary=bin/linux-arm64/mock-db  service=db
... [starlark] target started  pid=...  service=db
... [starlark] starting session  binary=bin/linux-arm64/mock-api  service=api
... [starlark] target started  pid=...  service=api
... [starlark] test completed  test=test_happy_path  result=pass

--- PASS: test_happy_path (200ms, seed=0) ---

1 passed, 0 failed
```

Notice: `depends_on = [db]` tells Faultbox to start db before api.
`env = {"DB_ADDR": db.main.addr}` wires the api to the db's address.

### How `db.main.addr` works

`db.main` is not a keyword — `"main"` is just the name you gave the interface.
It returns an `InterfaceRef` with properties like `.addr`, `.port`, `.host`.
Step methods (`.get()`, `.send()`, `.query()`) come from the protocol plugin.

Nothing here is magic — `"main"`, `"public"`, `"api"` are your names.
The protocol string (`"http"`, `"tcp"`, `"postgres"`) selects which plugin
provides the methods.

> For the full type reference (Service, Interface, InterfaceRef, Response,
> StarlarkEvent, all protocol methods), see
> [Spec Language Reference — Type Reference](../../spec-language.md#type-reference).

**The pattern:** topology is declared, not discovered. You're writing down
"the api depends on the db at this address" — making the architecture explicit.

> **Cyclic dependencies:** `depends_on` controls **startup order** and must be
> acyclic — Faultbox will reject `A depends_on B, B depends_on A`. But `env`
> wiring is unrestricted: services can exchange each other's addresses and call
> each other at runtime. Pick one direction for `depends_on` (whichever service
> needs to be healthy first), and wire the rest through `env`.

## The `fault()` builtin

`fault()` applies temporary faults while running a callback. Add this
test to your `fault-test.star`:

```python
def test_db_write_failure():
    """DB writes fail with I/O error — API should return 500."""
    def scenario():
        resp = api.post(path="/data/failkey", body="value")
        assert_true(resp.status >= 500, "expected 5xx on DB write failure")
    fault(db, write=deny("EIO", label="disk failure"), run=scenario)
```

Run it:
```bash
# Linux:
bin/faultbox test fault-test.star --test db_write_failure
# macOS (Lima):
vm bin/linux-arm64/faultbox test fault-test.star --test db_write_failure
```

```
--- PASS: test_db_write_failure (200ms, seed=0) ---
  syscall trace (85 events):
    #72  db    write   deny(input/output error)  [disk failure]
    #73  db    write   deny(input/output error)  [disk failure]
  fault rule on db: write=deny(EIO) → filter:[write,writev,pwrite64] label="disk failure"
```

The `label="disk failure"` tag appears in the trace output as `[disk failure]`
next to each faulted syscall — making it easy to identify which fault rule
caused each denial when you have multiple faults active.

What happens:
1. **Apply** `write=deny("EIO")` to the db service's seccomp filter
2. **Run** `scenario()` — which exercises the fault
3. **Remove** the fault (even if scenario fails)

The fault is **scoped** — it exists only during the callback. This is
important: you're testing a specific hypothesis ("when db writes fail,
the api returns 500"), not creating permanent damage.

### Why scoping is powerful

The basic example above tests one failure mode. But scoping lets you do
much more — test **setup → break → verify recovery** in a single test:

Add to `fault-test.star`:

```python
def test_recovery_after_disk_error():
    # Setup: write data while DB is healthy.
    api.post(path="/data/key1", body="safe-value")

    # Break: DB disk fails during this window only.
    def break_writes():
        resp = api.post(path="/data/key2", body="doomed")
        assert_true(resp.status >= 500, "should fail during disk error")
    fault(db, write=deny("EIO"), run=break_writes)

    # Verify: DB is healthy again — old data survived.
    resp = api.get(path="/data/key1")
    assert_eq(resp.body, "safe-value")
```

Run it:
```bash
# Linux:
bin/faultbox test fault-test.star --test recovery_after_disk_error
# macOS (Lima):
vm bin/linux-arm64/faultbox test fault-test.star --test recovery_after_disk_error
```

```
--- PASS: test_recovery_after_disk_error (350ms, seed=0) ---
  fault rule on db: write=deny(EIO) → filter:[write,writev,pwrite64]
    ↑ fault active only during break_writes(), removed after
```

Or test **multiple failure modes in sequence**. Add to `fault-test.star`:

```python
def test_db_failure_modes():
    # Disk full — does the API say "disk full" or just "error"?
    def check_enospc():
        resp = api.post(path="/data/k", body="v")
        assert_true(resp.status >= 500, "expected 5xx on disk full")
    fault(db, write=deny("ENOSPC"), run=check_enospc)

    # Slow disk — does the API timeout or wait?
    def check_slow():
        resp = api.post(path="/data/k", body="v")
        assert_true(resp.status in [200, 504], "expected success or gateway timeout")
    fault(db, write=delay("2s"), run=check_slow)
```

Run it:
```bash
# Linux:
bin/faultbox test fault-test.star --test db_failure_modes
# macOS (Lima):
vm bin/linux-arm64/faultbox test fault-test.star --test db_failure_modes
```

Without scoping, you'd need to restart services between tests to change
fault rules. Scoped faults give you clean transitions for free.

## deny() and delay()

Two ways to break things:

```python
# Fail immediately with an error code
deny("EIO")                    # I/O error, 100%
deny("ENOSPC")                 # disk full
deny("ECONNREFUSED")           # connection refused
deny("EIO", probability="50%") # 50% chance
deny("EIO", label="WAL write") # labeled — shows [WAL write] in trace

# Slow down, then allow
delay("500ms")                 # add 500ms latency
delay("2s", probability="30%") # 30% chance of 2s delay
delay("500ms", label="slow disk") # labeled for trace clarity
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

> **What if a variant is missing?** The family expansion covers the most
> common variants (Postgres, Redis, Go, Java). But some databases use
> less common syscalls — e.g., RocksDB may use `pwritev2`, MySQL InnoDB
> uses `sync_file_range`. If your fault is applied but never fires, check
> the diagnostic output (see "Reading diagnostic output" below) — the
> per-service syscall summary shows exactly which syscalls the service
> actually used. You can always target the exact syscall directly:
> `fault(db, pwritev2=deny("EIO"), run=fn)`.

## Named operations

Instead of thinking in syscalls, you can define **named operations** that
group related syscalls:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
    ops = {
        "persist": op(syscalls=["write", "fsync"]),
    },
)

# Now use the operation name in fault():
def test_persist_failure():
    def scenario():
        resp = api.post(path="/data/key", body="val")
        assert_true(resp.status >= 500, "expected 5xx on persist failure")
    fault(db, persist=deny("EIO", label="persist failure"), run=scenario)
```

The trace output shows the operation name: `persist(write) deny(EIO)`.

Operations can also include a **path filter** — useful for services that
write to specific files. The inventory-svc from Chapters 4-6 writes a
WAL file to `/tmp/inventory.wal`. Here's a working example:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

inventory = service("inventory", BIN + "/inventory-svc",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
    ops = {
        "wal_write": op(syscalls=["write", "fsync"], path="/tmp/*.wal"),
    },
)

orders = service("orders", BIN + "/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

def test_wal_write_failure():
    """Only WAL writes fail — inventory can still respond with an error."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 409, "expected 409 conflict on WAL failure")
    fault(inventory, wal_write=deny("EIO", label="WAL broken"), run=scenario)
```

With a path filter, only writes to matching files are faulted — stdout,
TCP responses, and other writes are unaffected. The inventory service
can still respond to the order-svc (409 Conflict) even though its WAL
write failed. This is the value of targeted faults — you test one
component's failure while the rest of the system keeps working.

> **When to use ops:** When you want to fault a logical operation (like
> "persist to disk") that involves multiple syscalls. For simple cases,
> raw syscall names work fine. See the
> [Spec Language Reference](../../spec-language.md#type-reference) for details.

## Multi-fault injection

Apply multiple faults at once to simulate cascading failures. Add to
`fault-test.star`:

```python
def test_everything_broken():
    def scenario():
        resp = api.post(path="/data/key", body="val")
        assert_true(resp.status >= 500, "expected 5xx when DB writes fail")
    fault(db,
        write=deny("EIO"),
        read=deny("EIO"),
        run=scenario,
    )
```

Run it:
```bash
# Linux:
bin/faultbox test fault-test.star --test everything_broken
# macOS (Lima):
vm bin/linux-arm64/faultbox test fault-test.star --test everything_broken
```

```
--- PASS: test_everything_broken (5213ms, seed=0) ---
  syscall trace (2 events):
    #8  db    write   deny(input/output error)
  fault rule on db: read=deny(EIO) → filter:[read,readv,pread64]
  fault rule on db: write=deny(EIO) → filter:[write,writev,pwrite64]
```

## Imperative fault control

For scenarios where fault timing matters, use `fault_start`/`fault_stop`
instead of the scoped `fault()` with `run=`.

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", BIN + "/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def test_fault_mid_operation():
    # Phase 1: no faults — write succeeds.
    resp = api.post(path="/data/key1", body="before")
    assert_eq(resp.status, 200)

    # Phase 2: enable fault — write fails.
    fault_start(db, write=deny("EIO"))
    resp = api.post(path="/data/key2", body="during")
    assert_true(resp.status >= 500, "expected error during fault")

    # Phase 3: disable fault.
    fault_stop(db)
```

Run it:
```bash
# Linux:
bin/faultbox test fault-test.star --test fault_mid_operation
# macOS (Lima):
vm bin/linux-arm64/faultbox test fault-test.star --test fault_mid_operation
```

```
--- PASS: test_fault_mid_operation (300ms, seed=0) ---
    #30  db    write   allow           ← Phase 1: no fault
    #45  db    write   deny(EIO)       ← Phase 2: fault active
```

> **Why no Phase 3 recovery check?** The mock-db crashes when it gets EIO —
> it doesn't recover within the same process. Real databases (Postgres, Redis)
> handle I/O errors more gracefully. In production testing, you'd verify
> recovery after `fault_stop()`. Here, verifying the fault fires is enough.

Prefer `fault()` with `run=` when possible — it guarantees cleanup even
if the test fails.

## Reading diagnostic output

When a fault fires:
```
--- PASS: test_db_write_failure (200ms) ---
  syscall trace (85 events):
    #72  db    write   deny(input/output error)
    #73  db    write   deny(input/output error)
  fault rule on db: write=deny(EIO) -> filter:[write,writev,pwrite64]
```

When a fault was applied but never fired:
```
  fault rule on db: fsync=deny(EIO) -> filter:[fsync,fdatasync]
  WARNING: fault rules were installed but no injections fired
    hint: the target may use a different syscall variant (e.g., pwrite64 instead of write)
    hint: run with --debug to see all intercepted syscalls
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
