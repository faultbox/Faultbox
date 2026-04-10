# Chapter 8: Scenarios & Failure Generation

**Duration:** 20 minutes
**Prerequisites:** [Chapter 3 (Fault Injection)](../02-syscall-level/03-fault-injection.md) completed

## Goals & Purpose

In chapters 2-7 you wrote fault tests by hand — picking specific failure
modes, writing assertions, running them. This works, but it's slow and
you inevitably miss failure modes.

**The question:** "Have I tested every way this system can break?"

This chapter teaches you to:
- **Register happy-path scenarios** — describe how your system works
- **Auto-generate failure mutations** — let Faultbox propose what can break
- **Review and curate** — keep the relevant tests, discard noise
- **Organize specs across files** — use `load()` for clean separation

After this chapter, your workflow becomes: write the happy path once,
generate all failures automatically, review, commit.

## The `scenario()` builtin

A scenario is a function that describes how your system works when
everything is healthy. Register it with `scenario()`:

```python
# Linux (native): BIN = "bin"
# macOS (Lima): BIN = "bin/linux"
BIN = "bin/linux"

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

def order_flow():
    """Write data through the API, read it back."""
    resp = api.post(path="/data/mykey", body="myvalue")
    assert_eq(resp.status, 200)
    resp = api.get(path="/data/mykey")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "myvalue")

scenario(order_flow)
```

Save this as `scenario-test.star`.

`scenario(order_flow)` does two things:
1. **Registers** the function for the failure generator
2. **Runs it as a test** — equivalent to naming it `test_order_flow`

Run it:
```bash
# Linux:
faultbox test scenario-test.star
# macOS (Lima):
vm faultbox test scenario-test.star
```

```
--- PASS: test_order_flow (200ms, seed=0) ---

1 passed, 0 failed
```

## Generating failure scenarios

Now generate all possible failures for your scenarios:

```bash
# Linux:
faultbox generate scenario-test.star
# macOS (Lima):
vm faultbox generate scenario-test.star
```

```
wrote order_flow.faults.star
```

Look at the generated file:

```python
# order_flow.faults.star (generated)
load("scenario-test.star", "api", "db", "order_flow")

# --- network failures ---

def test_gen_order_flow_db_down():
    """order_flow with db connection refused."""
    fault(api, connect=deny("ECONNREFUSED", label="db down"), run=order_flow)

def test_gen_order_flow_db_slow():
    """order_flow with db delayed 5s."""
    fault(api, connect=delay("5s", label="db slow"), run=order_flow)

def test_gen_order_flow_db_reset():
    """order_flow with db dropping mid-request."""
    fault(api, read=deny("ECONNRESET", label="db connection reset"), run=order_flow)

def test_gen_order_flow_db_partition():
    """order_flow with network partition between api and db."""
    partition(api, db, run=order_flow)

# --- disk failures ---

def test_gen_order_flow_db_io_error():
    """order_flow with db disk I/O error."""
    fault(db, write=deny("EIO", label="disk I/O error"), run=order_flow)

def test_gen_order_flow_db_disk_full():
    """order_flow with db disk full."""
    fault(db, write=deny("ENOSPC", label="disk full"), run=order_flow)

def test_gen_order_flow_db_fsync_fail():
    """order_flow with db fsync failure."""
    fault(db, fsync=deny("EIO", label="fsync failure"), run=order_flow)
```

**What happened:** the generator took your `order_flow` function and
wrapped it in every fault applicable to the `api → db` dependency.
No invented API calls, no guessed assertions — your exact happy path
under different failure conditions.

## Running generated tests

```bash
# Linux:
faultbox test order_flow.faults.star
# macOS (Lima):
vm faultbox test order_flow.faults.star
```

Some tests will pass (your system handles the fault), others will fail
(your happy-path assertions fail under fault — expected). For each failure:

- **Expected failure** → the system doesn't handle this fault mode.
  Either fix the code or adjust the assertion.
- **Unexpected pass** → the system gracefully handles this fault. Great!
- **Irrelevant** → delete the test (e.g., fsync on a service that
  doesn't use fsync).

## The `load()` statement

Generated files use `load()` to import topology and scenario functions:

```python
load("scenario-test.star", "api", "db", "order_flow")
```

This means:
- Services are defined once (in your source file)
- Generated files share the same topology
- You can regenerate without affecting your source
- Multiple files can load from the same source

You can also use `load()` in hand-written files:

```python
# my-custom-failures.star
load("scenario-test.star", "api", "db")

def test_custom_failure():
    """My specific failure scenario."""
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_true(resp.status >= 500, "should fail")
    fault(db, write=deny("EIO"), run=scenario)
```

## Dry run — preview without generating

```bash
# Linux:
faultbox generate scenario-test.star --dry-run
# macOS (Lima):
vm faultbox generate scenario-test.star --dry-run
```

```
  order_flow × api: 4 mutations
  order_flow × db: 3 mutations
  Total: 7 mutations
```

## Multiple scenarios

Register as many scenarios as you want — each gets its own `.faults.star`:

```python
def order_flow():
    api.post(path="/data/mykey", body="myvalue")
    resp = api.get(path="/data/mykey")
    assert_eq(resp.body, "myvalue")

def health_check():
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)

scenario(order_flow)
scenario(health_check)
```

```bash
faultbox generate scenario-test.star
# → order_flow.faults.star
# → health_check.faults.star
```

## Monitors and assertions in scenarios

Scenarios are full test functions — use monitors, trace assertions,
and parallel operations:

```python
def order_with_verification():
    # Monitor: fail if any write is denied during happy path.
    monitor(lambda e: fail("unexpected denial") if e.decision.startswith("deny"),
        service="db", syscall="write")

    # Trace db writes for temporal assertions.
    trace_start(db, syscalls=["write"])

    api.post(path="/data/key1", body="value1")
    resp = api.get(path="/data/key1")
    assert_eq(resp.body, "value1")

    # Verify write happened before read response.
    assert_eventually(service="db", syscall="write")

    trace_stop(db)

scenario(order_with_verification)
```

When the generator wraps this in a fault scope, the monitor and assertions
fire under fault — catching real bugs.

## The workflow

```
1. Write scenario() functions — describe how things work
2. faultbox generate → creates <scenario>.faults.star files
3. faultbox test *.faults.star → run all failures
4. Review: keep, adjust, or delete each generated test
5. Commit both source and curated .faults.star files
6. Regenerate when topology changes (new service, new dependency)
```

## What you learned

- `scenario(fn)` registers a happy path — runs as test + available to generator
- `faultbox generate` creates one `.faults.star` per scenario
- Generated tests wrap your exact happy path in fault scopes
- `load()` imports topology and functions across files
- Monitors and assertions work inside scenarios

## What's next

You've automated failure discovery. But so far you're only observing
syscall events. Chapter 9 introduces **event sources** — capturing
structured stdout, database WAL changes, and message queue events
as first-class trace data.
