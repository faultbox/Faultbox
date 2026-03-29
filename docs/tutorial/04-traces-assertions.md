# Chapter 4: Traces & Assertions

**Duration:** 25 minutes
**Prerequisites:** [Chapter 0 (Setup)](00-setup.md) completed

## Goals & Purpose

In chapters 2-3 you tested inputs and outputs: "I sent this request, I got
this response." But distributed systems bugs live in the *middle* — in the
sequence of internal operations between request and response.

Consider: your API returns 200, but did it actually write to the WAL? Or did
it skip the write and respond from cache? Both return 200, but one is
correct and the other is a data loss bug.

**The key insight:** to verify distributed systems correctly, you need to
assert on **what happened inside**, not just what came out. Faultbox records
every intercepted syscall as an event — giving you a complete audit trail of
the system's internal behavior.

This chapter teaches you to:
- **Query the syscall trace** — "did this operation happen?"
- **Assert temporal properties** — "A happened before B"
- **Prove absence** — "this operation did NOT happen"
- **Visualize causality** — see which service did what, and when

After this chapter, you'll write tests that don't just check outputs — they
verify the *mechanism* that produced them.

## The demo system

Chapters 4-6 use the full demo: an order service that talks to an inventory
service with a write-ahead log (WAL).

Binaries were built in Chapter 0. Run the demo (on macOS: prefix with `vm`):

```bash
bin/faultbox test poc/demo/faultbox.star
```

## The event log

Every intercepted syscall is recorded with:
- **Sequence number** — global ordering
- **Timestamp** — when it happened
- **Service name** — which component
- **Syscall, PID, decision** — what was intercepted and what faultbox decided
- **File path** — for file syscalls
- **Vector clock** — logical time per service (for causality)

Even `allow` decisions are recorded. The trace is a complete record.

## assert_eventually — "this happened"

The most common temporal assertion: verify something occurred.

```python
def test_wal_written():
    """Order placement triggers a WAL write."""
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)

    # Prove: the inventory service opened the WAL file.
    assert_eventually(
        service="inventory",
        syscall="openat",
        path="/tmp/inventory.wal",
    )
```

**Why this matters:** the API returned 200, but `assert_eventually` proves
the write actually reached the WAL. Without this, you're only testing the
happy-path HTTP response — not the durability guarantee.

## assert_never — "this didn't happen"

Prove that something did NOT occur — equally important for correctness:

```python
def test_no_wal_when_unreachable():
    """When inventory is unreachable, no WAL write should occur."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # Prove: the WAL was never touched.
        assert_never(
            service="inventory",
            syscall="openat",
            path="/tmp/inventory.wal",
        )
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)
```

**Why this matters:** if the order failed, no WAL write should have happened.
If one did, there's a bug: the system partially wrote data for a failed order.

## assert_before — ordering guarantees

Verify that events happened in the correct order:

```python
def test_wal_before_confirmation():
    """WAL must be written before order is confirmed."""
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)

    assert_before(
        first={"service": "inventory", "syscall": "openat", "path": "/tmp/inventory.wal"},
        then={"service": "inventory", "syscall": "write"},
    )
```

**Why this matters:** if the write happens before the WAL open, the system
has a durability bug — it confirmed before persisting. `assert_before` catches
this regardless of timing.

## Filter parameters

All temporal assertions accept the same filters:

| Parameter | Matches | Supports glob |
|-----------|---------|---------------|
| `service` | Service name | No |
| `syscall` | Syscall name | No |
| `path` | File path | No |
| `decision` | Fault decision | Yes: `"deny*"` matches any denial |

Glob examples: `decision="deny*"` matches `"deny(EIO)"`, `"deny(ENOSPC)"`, etc.

## events() — query for custom logic

Get matching events as a list for advanced assertions:

```python
def test_retry_count():
    """Count connection retries under flaky network."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        retries = events(service="orders", syscall="connect", decision="deny*")
        print("connection retries:", len(retries))
        assert_true(len(retries) < 10, "too many retries")
    fault(orders, connect=deny("ECONNREFUSED", probability="50%"), run=scenario)
```

## Trace output formats

### JSON trace — for programmatic analysis

```bash
faultbox test poc/demo/faultbox.star --output trace.json
```

Structured JSON with every event, PObserve-compatible fields, vector clocks,
and replay commands for failed tests.

### ShiViz — for visual causality

```bash
faultbox test poc/demo/faultbox.star --shiviz trace.shiviz
```

Open at https://bestchai.bitbucket.io/shiviz/ to see a space-time diagram
with arrows between services. You'll see exactly when each service acted
and how their operations interleaved.

### Normalized trace — for determinism verification

```bash
faultbox test poc/demo/faultbox.star --normalize trace1.norm
faultbox test poc/demo/faultbox.star --normalize trace2.norm
faultbox diff trace1.norm trace2.norm
```

Same seed + same binary = identical normalized trace. This proves your
system is deterministic under the same inputs.

## What you learned

- `assert_eventually()` proves something happened in the syscall trace
- `assert_never()` proves something did NOT happen
- `assert_before()` proves ordering between events
- `events()` returns matching events for custom logic
- Trace output: JSON (programmatic), ShiViz (visual), normalized (determinism)

**The mental model:** don't just test inputs and outputs. Test the mechanism:
"the WAL was written before the response," "no data was persisted for a
failed operation," "retries happened fewer than N times."

## What's next

Your tests verify behavior under specific scenarios. But distributed systems
bugs often depend on *timing* — which request arrives first, which write
completes first. The same code can work in one ordering and fail in another.

Chapter 5 introduces `parallel()` — running operations concurrently and
exploring different interleavings to find timing-dependent bugs.

## Exercises

1. **WAL ordering**: Write a test that places an order and asserts:
   - The WAL was opened (`assert_eventually` with openat + path)
   - The WAL was written to (`assert_eventually` with write)
   - The open happened before the write (`assert_before`)

2. **Clean happy path**: Write a happy-path test and assert zero denied
   syscalls:
   ```python
   denied = events(decision="deny*")
   assert_eq(len(denied), 0, "no syscalls should be denied")
   ```

3. **Connection count**: Write a test that counts `connect` syscalls from
   the orders service. Is it 1 per request? More? Why?

4. **ShiViz visualization**: Run with `--shiviz trace.shiviz` and open
   the file in ShiViz. Find the `vector_clock` arrows. What does a merge
   arrow between two services represent?
