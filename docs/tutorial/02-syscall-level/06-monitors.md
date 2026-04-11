# Chapter 6: Monitors & Partitions (Preview)

**Duration:** 10 minutes
**Prerequisites:** [Chapter 5 (Concurrency)](05-concurrency.md) completed

## Quick introduction

Previous chapters tested specific scenarios. But distributed systems need
**invariants** — properties that must hold regardless of the scenario.

**Monitors** let you express these invariants as code that runs continuously
during every test:

```python
def no_negative_stock(event):
    if (event.data.get("stock") is not None
            and int(event.data["stock"]) < 0):
        fail("stock went negative!")

monitor(no_negative_stock, service="inventory")
```

**Network partitions** simulate bidirectional network splits:

```python
def test_partition():
    def scenario():
        resp = orders.post(path="/orders", body='...')
        assert_eq(resp.status, 503)
    partition(orders, inventory, run=scenario)
```

## Deep dive in Part 4

This chapter is a preview. For the complete treatment of monitors,
invariants, temporal properties, and partitions, see **Part 4: Safety
& Verification**:

- [Chapter 14: Invariants & Safety Properties](../04-safety/14-invariants.md) —
  safety vs liveness, finding invariants, expressing them in Faultbox
- [Chapter 15: Monitors & Temporal Properties](../04-safety/15-monitors.md) —
  monitor lifecycle, temporal ordering, combined with event sources
- [Chapter 16: Network Partitions](../04-safety/16-partitions.md) —
  bidirectional splits, partition + monitor, recovery testing

## What's next

You've completed Part 2 (Syscall-Level). Continue to:
- [Part 3: Protocol-Level Faults](../03-protocol-level/index.md) — HTTP, database, broker faults
- [Part 4: Safety & Verification](../04-safety/index.md) — invariants, monitors, partitions (deep)
