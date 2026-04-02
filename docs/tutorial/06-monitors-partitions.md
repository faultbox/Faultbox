# Chapter 6: Monitors & Network Partitions

**Duration:** 20 minutes
**Prerequisites:** [Chapter 0 (Setup)](00-setup.md) completed

## Goals & Purpose

Previous chapters tested specific scenarios: "inject this fault, assert that
response." But distributed systems need **invariants** — properties that must
hold regardless of the scenario.

Example: "No order should be confirmed when inventory is unreachable." This
isn't a single test case — it's a rule that must hold across all failure
modes, all interleavings, all timing combinations.

**Monitors** let you express these invariants as code that runs continuously
during every test. They watch the syscall stream and fail immediately if an
invariant is violated.

**Network partitions** are a specific failure mode that's hard to simulate
with basic `fault()`: a split between two specific services, while other
connectivity remains intact. In production, network partitions are the most
dangerous failure mode — they cause split-brain, data inconsistency, and
cascading failures.

This chapter teaches you to:
- **Define safety properties** as monitors that run continuously
- **Simulate network partitions** between specific services
- **Combine monitors + partitions** to verify invariants under failure
- **Think in safety vs liveness** — "bad things don't happen" vs "good things eventually do"

## Setup

This chapter uses the same two-service system from Chapters 4-5:

```
orders (HTTP :8080) ──→ inventory (TCP :5432 + WAL file)
```

Create `monitors-test.star` in the project root:

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

Add all monitors and test functions from this chapter to this file.

## Monitors

A monitor is a callback that fires on every matching syscall event:

```python
def no_unhandled_errors(event):
    """Safety: every denied syscall should be handled."""
    if event["decision"].startswith("deny"):
        # The application should handle this — if it doesn't,
        # the next assertion will catch the cascading failure.
        pass

monitor(no_unhandled_errors, service="inventory", syscall="write")
```

Monitors are registered before tests and cleared between tests.
If a monitor callback raises an error, the test fails immediately with
"monitor violation."

## Practical monitor: denial limit

```python
deny_count = {"n": 0}

def limit_denials(event):
    """No more than 5 denied writes — beyond that, the system should fail-fast."""
    if event["decision"].startswith("deny"):
        deny_count["n"] += 1
        if deny_count["n"] > 5:
            fail("too many denied writes: " + str(deny_count["n"]))

monitor(limit_denials, service="inventory", decision="deny*")
```

**Why this matters:** in production, a failing disk doesn't cause 1 error — it
causes hundreds. If your system retries indefinitely, those hundreds become
thousands. A denial limit monitor catches this: "if you've failed 5 times,
stop trying and report the error."

## Monitor filters

Same filters as temporal assertions:

```python
monitor(callback, service="inventory")                    # all inventory syscalls
monitor(callback, service="inventory", syscall="write")   # inventory writes only
monitor(callback, decision="deny*")                       # any denied syscall anywhere
monitor(callback, service="orders", syscall="connect")    # orders' connections
```

## Network partitions

`partition()` creates a bidirectional network split between two services:

```python
def test_network_partition():
    """Orders can't reach inventory — should return 503."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # Inventory was never contacted.
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")

    partition(orders, inventory, run=scenario)
```

Run it:
```bash
# Linux:
bin/faultbox test monitors-test.star --test network_partition
# macOS (Lima):
vm bin/linux-arm64/faultbox test monitors-test.star --test network_partition
```

```
--- PASS: test_network_partition (200ms, seed=0) ---
    #12  orders  connect  deny(connection refused)  → inventory:8081
```

During the partition:
- `orders` can't `connect()` to `inventory` → ECONNREFUSED
- `inventory` can't `connect()` to `orders` → ECONNREFUSED
- Other connectivity (orders to external APIs) is **not affected**

## partition() vs fault()

| | `fault(svc, connect=deny(...))` | `partition(svc_a, svc_b)` |
|-|---------------------------------|---------------------------|
| **Scope** | ALL connections from svc | Only between svc_a and svc_b |
| **Direction** | One-way | Bidirectional |
| **Other connectivity** | Blocked | Preserved |
| **Use case** | "Service is completely isolated" | "Network split between two specific services" |

**The intuition:** in production, network partitions are usually selective.
Your API can reach the load balancer but not the database. `partition()` models
this precisely.

## Combining monitors and partitions

The real power: define an invariant, then stress-test it:

```python
# Safety property: no data should be written when inventory is unreachable.
def no_phantom_writes(event):
    if event["syscall"] == "openat" and "/tmp/inventory.wal" in event.get("path", ""):
        fail("WAL was accessed during partition — data inconsistency risk")

monitor(no_phantom_writes, service="inventory")

def test_partition_safety():
    """During partition: no order confirmed, no WAL written."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status != 200, "order must not succeed during partition")
    partition(orders, inventory, run=scenario)
```

Run it:
```bash
# Linux:
bin/faultbox test monitors-test.star --test partition_safety
# macOS (Lima):
vm bin/linux-arm64/faultbox test monitors-test.star --test partition_safety
```

```
--- PASS: test_partition_safety (200ms, seed=0) ---
    #12  orders  connect  deny(connection refused)  → inventory:8081
```

This test says: "when orders and inventory are split, orders must not confirm,
and inventory's WAL must not be touched." The monitor enforces the second
property continuously.

## What you learned

- `monitor(callback, ...)` defines invariants checked on every syscall event
- Monitors fail immediately on violation — no waiting for the test to finish
- `partition(a, b)` creates targeted, bidirectional network splits
- Partitions preserve other connectivity (more realistic than blanket deny)
- Monitors + partitions = safety property verification under failure

**The framework:** for any distributed system, identify:
1. **Safety properties** — bad things that must never happen (data loss, inconsistency)
2. **Failure modes** — network partitions, disk failures, slow responses
3. **Write monitors** for the safety properties
4. **Write partition/fault tests** for the failure modes
5. **Combine** — verify safety holds under all failure modes

## What's next

Everything so far uses mock binaries — lightweight, fast, but not real
infrastructure. In production, you use Postgres, Redis, Kafka — each with
their own syscall patterns, startup sequences, and failure behaviors.

Chapter 7 shows how to test real Docker containers with the same fault
injection tools. Same `fault()` API, same assertions, real infrastructure.

## Exercises

1. **Denial counter**: Write a monitor that counts denied `write` syscalls
   on inventory. Run a test with `write=deny("EIO")`. How many denials
   occur for a single order?

2. **Partition + assertion**: Write a test that partitions orders from
   inventory, then asserts:
   - `assert_never(service="inventory", syscall="openat")` — not contacted
   - Response is 503

3. **Selective partition**: Partition orders from inventory, but also hit
   the `/health` endpoint. Does health still return 200? (It should —
   health doesn't need inventory.)

4. **Monitor as guardrail**: Write a monitor that fails if any `connect`
   is *allowed* during a partition test. Run
   `partition(orders, inventory, ...)` and verify the monitor catches
   ECONNREFUSED (denied), not allowed connections.
