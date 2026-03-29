# Chapter 4: Traces & Assertions

In chapter 3 you broke things. Now you'll verify *exactly what happened* at the
syscall level — which files were opened, which connections were made, and in
what order.

## The demo system

Chapters 4-6 use the full demo: an order service that talks to an inventory
service with a write-ahead log (WAL).

```bash
make demo-build  # builds order-svc + inventory-svc for Lima VM
```

The demo spec is at `poc/demo/faultbox.star`. Run it:
```bash
faultbox test poc/demo/faultbox.star
```

## The event log

Every intercepted syscall is recorded as an **event** with:
- Sequence number
- Timestamp
- Service name
- Syscall name, PID, decision, file path
- Vector clock (for causality tracking)

Even `allow` decisions are recorded. The trace is a complete record of
what the system did at the kernel level.

## Temporal assertions

### assert_eventually — "this happened"

Verify that a syscall event occurred during the test:

```python
def test_wal_written():
    """Order placement triggers a WAL write."""
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)

    # The inventory service should have opened the WAL file.
    assert_eventually(
        service="inventory",
        syscall="openat",
        path="/tmp/inventory.wal",
    )
```

`assert_eventually` searches the test's event log for a matching event.
If none found, the test fails.

### assert_never — "this didn't happen"

Verify that something did NOT occur:

```python
def test_no_wal_when_unreachable():
    """When inventory is unreachable, no WAL write should occur."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # No WAL access should have happened.
        assert_never(
            service="inventory",
            syscall="openat",
            path="/tmp/inventory.wal",
        )
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)
```

### assert_before — ordering guarantees

Verify that events happened in the right order:

```python
def test_wal_before_response():
    """WAL must be opened before order is confirmed."""
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)

    assert_before(
        first={"service": "inventory", "syscall": "openat", "path": "/tmp/inventory.wal"},
        then={"service": "inventory", "syscall": "write"},
    )
```

### Filter parameters

All temporal assertions accept the same filter keywords:

| Parameter | Matches | Example |
|-----------|---------|---------|
| `service` | Service name | `"inventory"` |
| `syscall` | Syscall name | `"openat"`, `"write"`, `"connect"` |
| `path` | File path | `"/tmp/inventory.wal"` |
| `decision` | Fault decision | `"allow"`, `"deny*"` |

Glob matching: `"deny*"` matches `"deny(EIO)"`, `"deny(ENOSPC)"`, etc.

### events() — query the trace

Get a list of matching events for custom logic:

```python
def test_count_retries():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        retries = events(service="orders", syscall="connect", decision="deny*")
        print("connection retries:", len(retries))
    fault(orders, connect=deny("ECONNREFUSED", probability="50%"), run=scenario)
```

## Trace output formats

### JSON trace

```bash
faultbox test poc/demo/faultbox.star --output trace.json
```

Produces structured JSON with every event, including:
- `event_type`: dotted notation (`"syscall.write"`, `"lifecycle.started"`)
- `partition_key`: service name (for PObserve integration)
- `vector_clock`: per-service logical clocks
- `replay_command`: for failed tests, the exact command to reproduce

### ShiViz visualization

```bash
faultbox test poc/demo/faultbox.star --shiviz trace.shiviz
```

Open at https://bestchai.bitbucket.io/shiviz/ to see a space-time diagram
with communication arrows between services.

### Normalized trace

```bash
faultbox test poc/demo/faultbox.star --normalize trace1.norm
faultbox test poc/demo/faultbox.star --normalize trace2.norm
faultbox diff trace1.norm trace2.norm
```

Normalized traces strip timestamps for deterministic comparison.
Same seed + same binary = same trace.

## What you learned

- `assert_eventually()` verifies something happened in the syscall trace
- `assert_never()` verifies something did NOT happen
- `assert_before()` verifies ordering between events
- `events()` returns matching events for custom logic
- `--output trace.json` for structured analysis
- `--shiviz trace.shiviz` for visual causality diagrams
- `--normalize` + `diff` for determinism verification

## Exercises

1. **WAL ordering**: Write a test that places an order and asserts:
   - The WAL file was opened (`assert_eventually` with openat)
   - The WAL was written to (`assert_eventually` with write)
   - The open happened before the write (`assert_before`)

2. **No denied syscalls**: Write a happy-path test and assert there are
   zero denied syscalls:
   ```python
   denied = events(decision="deny*")
   assert_eq(len(denied), 0, "no syscalls should be denied in happy path")
   ```

3. **Count connections**: Write a test that counts how many `connect`
   syscalls the orders service made. Is it 1? More? Why?

4. **JSON analysis**: Run with `--output trace.json` and look at the JSON.
   Find the `vector_clock` field. What does `{"inventory": 5, "orders": 3}`
   mean?
