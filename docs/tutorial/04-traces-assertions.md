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

Chapters 4-6 use the full demo: two services that form a simple order
processing pipeline:

```
orders (HTTP :8080) ──→ inventory (TCP :5432 + WAL file)
```

- **orders** — HTTP API that receives order requests and forwards them
  to inventory
- **inventory** — TCP service that manages stock and writes a
  write-ahead log (WAL) to `/tmp/inventory.wal`

Create `traces-test.star` in the project root:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

inventory = service("inventory", BIN + "/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders", BIN + "/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)
```

**You'll add all test functions from this chapter to this file.**

Verify the topology works with a quick happy-path test — add to `traces-test.star`:

```python
def test_happy_path():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
```

Run it:
```bash
# Linux:
bin/faultbox test traces-test.star --test happy_path
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --test happy_path
```

```
--- PASS: test_happy_path (200ms, seed=0) ---
1 passed, 0 failed
```

Good — the topology works. Now add the trace assertion tests below.

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
    """Slow writes — but WAL write still happened."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 200)

        # Prove: the inventory service wrote specifically to the WAL file.
        assert_eventually(service="inventory", syscall="write", path="*.wal")
    fault(inventory, write=delay("100ms", label="slow WAL"), run=scenario)
```

The `path="*.wal"` filter checks not just "a write happened" but "a write
to a `.wal` file happened." Without the path check, you'd also match writes
to stdout or TCP sockets — which don't prove durability.

All filter parameters support glob patterns: `path="*.wal"`, `path="/tmp/*"`,
`decision="deny*"`. For more complex conditions, use `where=lambda`:

```python
# Lambda for conditions that globs can't express:
assert_eventually(
    where=lambda e: e.service == "inventory"
        and e.syscall == "write"
        and int(e.fields.get("size", "0")) > 1024
)
```

> **Why a fault here?** Trace assertions query the syscall event log.
> Events are only recorded for syscalls with a seccomp filter installed.
> `fault(inventory, write=delay(...))` installs filters for
> `[write,writev,pwrite64]` — so only write-family events appear in the
> trace. The fd→path resolution (via `/proc/PID/fd`) gives us the file
> path for each write, making the path filter possible.

Run it:
```bash
# Linux:
bin/faultbox test traces-test.star --test wal_written
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --test wal_written
```

```
--- PASS: test_wal_written (500ms, seed=0) ---
  syscall trace (4 events):
    #8  inventory  write  delay(100ms)  [slow WAL]  (+100ms)
  fault rule on inventory: write=delay(100ms) → filter:[write,writev,pwrite64] label="slow WAL"
```

**Why this matters:** the API returned 200, but `assert_eventually` proves
the write actually happened. Without this, you're only testing the
happy-path HTTP response — not the durability guarantee.

## assert_never — "this didn't happen"

Prove that something did NOT occur — equally important for correctness:

```python
def test_no_write_when_unreachable():
    """When inventory is unreachable, no denied connect should go through."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # Prove: every connect from orders was denied.
        assert_never(
            service="orders",
            syscall="connect",
            decision="allow",
        )
    fault(orders, connect=deny("ECONNREFUSED", label="inventory unreachable"), run=scenario)
```

> **Why not `assert_never(service="inventory", syscall="write")`?**
> Inventory has background writes (startup logs, healthcheck responses)
> that are unrelated to order processing. Since we can't path-filter
> writes (see Chapter 1 callout), we assert on the **cause** (connect
> denied) rather than the **effect** (no writes).

Run it:
```bash
# Linux:
bin/faultbox test traces-test.star --test no_write_when_unreachable
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --test no_write_when_unreachable
```

```
--- PASS: test_no_write_when_unreachable (200ms, seed=0) ---
  fault rule on orders: connect=deny(ECONNREFUSED) → filter:[connect]
    #8  orders  connect  deny(connection refused)
```

**Why this matters:** `assert_never` with `decision="allow"` proves that
no connection succeeded. If any did, inventory might have received a
partial request — a potential data inconsistency bug.

## assert_before — ordering guarantees

Verify that events happened in the correct order:

```python
def test_delay_then_deny():
    """Slow writes come before the fsync denial."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status != 200, "expected failure on fsync deny")

        assert_before(
            first={"service": "inventory", "syscall": "write"},
            then={"service": "inventory", "decision": "deny*"},
        )
    fault(inventory, write=delay("100ms", label="slow WAL"), fsync=deny("EIO", label="sync failure"), run=scenario)
```

> **Note:** Both fault keywords are in a single `fault()` call, so both
> filters are installed together. `assert_before` verifies that the
> delayed write happened before the denied fsync — confirming the WAL
> write attempt preceded the sync failure.

Run it:
```bash
# Linux:
bin/faultbox test traces-test.star --test delay_then_deny
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --test delay_then_deny
```

```
--- PASS: test_delay_then_deny (350ms, seed=0) ---
  assert_before: inventory.write → inventory.fsync(deny) ✓
  fault rule on inventory: write=delay(100ms), fsync=deny(EIO)
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
def test_write_count():
    """Count how many writes inventory makes for a single order."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 200)

        writes = events(service="inventory", syscall="write")
        print("inventory writes per order:", len(writes))
        assert_true(len(writes) >= 1, "expected at least 1 WAL write")
        assert_true(len(writes) < 10, "too many writes for a single order")
    fault(inventory, write=delay("10ms"), run=scenario)
```

`events()` returns a list of matching events. You can filter by any
combination of `service`, `syscall`, `decision`, `path` — then use
`len()`, loop, or inspect individual fields.

Run it:
```bash
# Linux:
bin/faultbox test traces-test.star --test write_count
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --test write_count
```

```
--- PASS: test_write_count (250ms, seed=0) ---
  inventory writes per order: 3
  fault rule on inventory: write=delay(10ms) → filter:[write,writev,pwrite64]
```

## Trace output formats

### JSON trace — for programmatic analysis

```bash
# Linux:
bin/faultbox test traces-test.star --output trace.json
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --output trace.json
```

Structured JSON with every event, PObserve-compatible fields, vector clocks,
and replay commands for failed tests.

### ShiViz — for visual causality

```bash
# Linux:
bin/faultbox test traces-test.star --shiviz trace.shiviz
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --shiviz trace.shiviz
```

Open at https://bestchai.bitbucket.io/shiviz/ to see a space-time diagram
with arrows between services. Each service gets its own swimlane, plus a
**"test"** swimlane representing the test driver (your step calls).

**What you'll see in the diagram:**
- **Causal arrows** between swimlanes — e.g., test sends a request to orders,
  orders connects to inventory
- **Fault labels** on events — `[slow WAL]`, `[sync failure]` — so you can
  identify which fault rule affected each syscall
- **Paths and latency** — file paths for I/O syscalls, `(+100ms)` for delays
- **VIOLATION markers** — if a test fails, a `VIOLATION [test_name] reason`
  event appears at the failure point in the "test" swimlane

### Normalized trace — for determinism verification

**The problem:** you fix a bug, and the tests pass. But did the fix change
the system's behavior in unexpected ways? Maybe the fix reordered internal
operations, or removed a retry, or changed which syscalls get called. The
tests still pass, but the system behaves differently — and you won't know
until production.

**The solution:** capture a normalized trace (stripped of timestamps,
PIDs, and other non-deterministic fields) before and after your change.
If the traces are identical, the behavior is unchanged. If they differ,
the diff shows exactly what changed.

**Step 1:** Capture a baseline trace:
```bash
# Linux:
bin/faultbox test traces-test.star --normalize trace-before.norm
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --normalize trace-before.norm
```

**Step 2:** Make your code change, rebuild, then capture again:
```bash
# Linux:
bin/faultbox test traces-test.star --normalize trace-after.norm
# macOS (Lima):
vm bin/linux-arm64/faultbox test traces-test.star --normalize trace-after.norm
```

**Step 3:** Compare:
```bash
# Linux:
bin/faultbox diff trace-before.norm trace-after.norm
# macOS (Lima):
vm bin/linux-arm64/faultbox diff trace-before.norm trace-after.norm
```

If the system is deterministic (same seed + same binary), the output is:
```
traces are identical
```

If the behavior changed, you see exactly what:
```
traces differ (24 vs 26 lines):
  line 12:
    run1: inventory write allow
    run2: inventory write allow
  line 13:
    run1: inventory fsync allow
    run2: inventory write allow        ← extra write before fsync
```

**When to use this:**
- Before/after a refactor — prove no behavioral change
- In CI — capture a baseline trace, fail if a PR changes it unexpectedly
- Debugging flaky tests — run twice with the same seed, diff to find non-determinism

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
