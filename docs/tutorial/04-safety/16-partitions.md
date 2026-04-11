# Chapter 16: Network Partitions

**Duration:** 20 minutes
**Prerequisites:** [Chapter 15 (Monitors)](15-monitors.md) completed

## Goals & Purpose

Network partitions are the most dangerous failure mode in distributed
systems. Unlike a crashed server (which is clearly down), a partition
creates **ambiguity** — each side thinks the other might be alive or dead.

This ambiguity causes split-brain: two nodes both think they're the
primary. Two order services both accept the last item in stock. Two
consumers both process the same message.

Faultbox simulates partitions by denying `connect` syscalls in both
directions. Combined with monitors from Chapter 15, you can verify that
your system handles partitions without violating safety invariants.

## partition() builtin

```python
def test_network_split():
    """Orders can't reach inventory — should return 503."""
    def scenario():
        resp = orders.post(path="/orders", body='...')
        assert_eq(resp.status, 503)
    partition(orders, inventory, run=scenario)
```

`partition(A, B, run=fn)` denies `connect` syscalls in both directions:
A can't connect to B, and B can't connect to A. All other connectivity
(healthchecks to themselves, other services) remains intact.

## Setup

```python
BIN = "bin/linux"

inventory = service("inventory", BIN + "/inventory-svc",
    interface("main", "tcp", 5432),
    env={"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck=tcp("localhost:5432"),
)

orders = service("orders", BIN + "/order-svc",
    interface("public", "http", 8080),
    env={"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on=[inventory],
    healthcheck=http("localhost:8080/health"),
)
```

Save as `partition-test.star`.

## Happy path first

```python
def test_happy_path():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
```

**Linux:**
```bash
faultbox test partition-test.star --test happy_path
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test partition-test.star --test happy_path"
```

## Basic partition test

```python
def test_partition_returns_error():
    """When orders can't reach inventory, it should return an error."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
    partition(orders, inventory, run=scenario)
```

**Linux:**
```bash
faultbox test partition-test.star --test partition_returns_error
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test partition-test.star --test partition_returns_error"
```

**What happened:** Faultbox installed `connect=deny("ECONNREFUSED")` on
both services. When orders tried to open a TCP connection to inventory,
the kernel returned ECONNREFUSED. Orders detected the failure and returned
503 to the caller.

## Partition + invariant monitor

The real value: verify that partitions don't violate safety properties.

```python
def test_no_stock_change_during_partition():
    """If orders can't reach inventory, stock must not change.

    This catches a subtle bug: if orders has a local cache of stock
    levels and decrements it optimistically before confirming with
    inventory, a partition could allow orders to "sell" items that
    inventory never reserved.
    """
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        # We don't care about the status — we care about the invariant.

        # No WAL write should happen on inventory (no reservation).
        assert_never(
            service="inventory",
            syscall="write",
            path="/tmp/inventory.wal",
        )

    partition(orders, inventory, run=scenario)
```

**Linux:**
```bash
faultbox test partition-test.star --test no_stock_change_during_partition
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test partition-test.star --test no_stock_change_during_partition"
```

## Partition + recovery

Test that the system recovers after the partition heals:

```python
def test_recovery_after_partition():
    """System works again after partition is resolved."""
    # Phase 1: partition — orders should fail.
    def during_partition():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
    partition(orders, inventory, run=during_partition)

    # Phase 2: partition resolved — orders should work.
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
```

**Linux:**
```bash
faultbox test partition-test.star --test recovery_after_partition
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test partition-test.star --test recovery_after_partition"
```

## Partial partition (one direction)

`partition()` is bidirectional. For a one-directional split (A can't
reach B, but B can still reach A), use `fault()` directly:

```python
def test_one_way_partition():
    """Orders can't reach inventory, but inventory can still function.

    This simulates asymmetric network failure — common when a firewall
    rule is misconfigured or a load balancer drops traffic in one direction.
    """
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # Inventory is fine — it just doesn't receive requests.
        # If another client could reach it directly, it would work.
    fault(orders, connect=deny("ECONNREFUSED", label="one-way partition"),
        run=scenario)
```

## When to use partitions vs faults

| Scenario | Use |
|---|---|
| Service A can't reach service B | `fault(A, connect=deny(...))` |
| Services A and B can't reach each other | `partition(A, B)` |
| Service A's disk fails | `fault(A, write=deny("EIO"))` |
| Network is slow between A and B | `fault(A, connect=delay("2s"))` |
| Total isolation of service A | `partition(A, B)` + `partition(A, C)` for all dependencies |

## What you learned

- `partition(A, B)` creates a bidirectional network split
- Partitions + monitors verify safety under network failure
- Test recovery after partition resolution
- One-way partitions use `fault()` with `connect=deny()`
- Partitions are the hardest failure mode — test them explicitly

## What's next

You've completed the Safety & Verification section. You can now:
- Define invariants that your system must always maintain
- Write monitors that verify them continuously
- Test under network partitions

Continue to [Part 5: Advanced](../05-advanced/index.md) for containers,
scenarios, event sources, named operations, and LLM integration.
