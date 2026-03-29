# Chapter 6: Monitors & Network Partitions

Previous chapters tested specific scenarios. In this chapter you'll define
**safety properties** that must hold across all tests, and simulate
**network partitions** between services.

## Monitors

A monitor is a callback that runs on every matching syscall event.
If it raises an error, the test fails with "monitor violation".

```python
def no_unhandled_errors(event):
    """Safety: denied writes should never go unlogged."""
    if event["decision"].startswith("deny"):
        # In a real system, you'd check that the application logged the error.
        pass

monitor(no_unhandled_errors, service="inventory", syscall="write")
```

Monitors are registered before tests run and cleared between tests.

## Practical monitor: deny count limit

```python
deny_count = {"n": 0}

def limit_denials(event):
    if event["decision"].startswith("deny"):
        deny_count["n"] += 1
        if deny_count["n"] > 5:
            fail("too many denied syscalls: " + str(deny_count["n"]))

monitor(limit_denials, service="inventory", decision="deny*")
```

This monitor fails the test if more than 5 syscalls are denied on the
inventory service. It's a safety property: "fault injection shouldn't
cause unbounded failures."

## Monitor filters

Monitors accept the same filters as temporal assertions:

```python
monitor(callback, service="inventory")                    # all inventory syscalls
monitor(callback, service="inventory", syscall="write")   # inventory writes only
monitor(callback, decision="deny*")                       # any denied syscall
monitor(callback, service="orders", syscall="connect")    # orders' connections
```

## Network partitions

`partition()` creates a bidirectional network split between two services:

```python
def test_network_partition():
    """Orders can't reach inventory — returns 503."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)

        # Verify: inventory was never contacted.
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")

    partition(orders, inventory, run=scenario)
```

During the partition:
- `orders` can't `connect()` to `inventory` (gets ECONNREFUSED)
- `inventory` can't `connect()` to `orders` (gets ECONNREFUSED)
- Other connectivity (orders to external APIs) is **not affected**

This is more precise than `fault(orders, connect=deny("ECONNREFUSED"))`,
which would block ALL outbound connections, not just to inventory.

## Partition vs fault

| | `fault(svc, connect=deny(...))` | `partition(svc_a, svc_b)` |
|-|---------------------------------|---------------------------|
| Scope | ALL connections from svc | Only connections between svc_a ↔ svc_b |
| Direction | One-way | Bidirectional |
| Other connectivity | Blocked | Preserved |
| Use case | "Service can't reach anything" | "Network split between two specific services" |

## Combining monitors and partitions

The real power: define a safety property, then create failure scenarios:

```python
# Safety property: no order should be confirmed when inventory is unreachable.
def no_phantom_orders(event):
    """If orders writes 'confirmed', inventory must have been reachable."""
    if event["syscall"] == "write" and "confirmed" in event.get("path", ""):
        # This is a simplified check. In practice, you'd verify the
        # event log shows a successful inventory connection.
        pass

monitor(no_phantom_orders, service="orders", syscall="write")

def test_partition_safety():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        # Order should fail, not succeed with phantom data.
        assert_true(resp.status != 200, "order should not succeed during partition")
    partition(orders, inventory, run=scenario)
```

## What you learned

- `monitor(callback, ...)` runs on every matching syscall event
- Monitors define safety properties that must hold during the test
- `partition(svc_a, svc_b, run=fn)` creates bidirectional network splits
- Partitions only affect connections between the two named services
- Monitors + partitions = verify safety properties under failure

## Exercises

1. **Denial counter**: Write a monitor that counts denied `write` syscalls
   on inventory. Then run a test with `write=deny("EIO")` and verify the
   monitor sees the denials. How many `write` denials occur for a single
   order?

2. **Partition + assertion**: Write a test that partitions orders from
   inventory, attempts an order, and then asserts:
   - `assert_never(service="inventory", syscall="openat")` — inventory
     was never contacted
   - The order returns 503

3. **Selective partition**: What happens if you partition orders from
   inventory, but then access orders' health endpoint? Does `/health`
   still work? (It should — health doesn't need inventory.)

4. **Monitor as invariant checker**: Write a monitor on the orders service
   that fails if any `connect` syscall is allowed during a partition test.
   Then run `partition(orders, inventory, run=scenario)` and verify the
   monitor catches the denied connections (not allowed ones).
