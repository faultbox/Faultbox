# Chapter 14: Invariants & Safety Properties

**Duration:** 30 minutes
**Prerequisites:** [Chapter 4 (Traces & Assertions)](../02-syscall-level/04-traces.md) completed

## Goals & Purpose

In chapters 2-8 you wrote tests that verify specific scenarios: "when X
happens, I expect Y." These tests are valuable, but they have a blind spot:
they only check the scenarios you thought of.

Distributed systems fail in ways you didn't predict. The database returns
an error you never tested. Two requests arrive in an order you didn't
consider. A retry fires at exactly the wrong moment.

**Invariants** solve this. An invariant is a property that must hold
**always** — not just in one test, but in every test, under every fault,
in every interleaving. If an invariant is violated, you've found a real
bug — regardless of which test triggered it.

This chapter teaches you to:
- **Distinguish tests from invariants** — and know when you need each
- **Understand safety vs liveness** — the two fundamental property types
- **Find invariants in your system** — systematic techniques
- **Express invariants in Faultbox** — using assertions and monitors

## Tests vs invariants

### A test verifies one scenario under one fault

In the domain-centric model ([Chapter 6](../02-syscall-level/06-domain-model.md)),
you define scenarios (what the system does) and fault assumptions (what can
go wrong) as separate layers, then compose them with `fault_matrix()`:

```python
def order_flow():
    return api.post(path="/orders", body='...')

scenario(order_flow)

db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
)

fault_matrix(
    scenarios = [order_flow],
    faults = [db_down],
    overrides = {
        (order_flow, db_down): lambda r: assert_eq(r.status, 503),
    },
)
```

This checks: "when the DB is down, the API returns 503." It says nothing
about what happens when the DB is slow, when the disk is full, or when
two requests race.

### An invariant verifies a property across ALL scenarios and faults

"An order confirmed to the user is always persisted in the database."

This must hold in every cell of the matrix:
- Every scenario (order flow, health check, bulk import, ...)
- Under every fault (DB down, slow, disk full, partition, ...)
- In every interleaving (concurrent requests, retries, ...)

If ANY combination violates this, you have a data loss bug.

In the domain-centric model, invariants live on **fault assumptions** as
monitors — they fire automatically in every test that uses the assumption:

```python
def order_persisted(event):
    # If API confirmed, DB must have the row
    if event["type"] == "stdout" and "confirmed" in event.get("body", ""):
        rows = events(where=lambda e: e.type == "wal" and e.data.get("action") == "INSERT")
        if len(rows) == 0:
            fail("order confirmed but not persisted!")

persistence_check = monitor(order_persisted)

db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
    monitors = [persistence_check],  # checked in every test using db_down
)
```

## Safety vs liveness

Formal verification divides properties into two categories.
Understanding the distinction helps you find invariants systematically.

### Safety: "bad things never happen"

A safety property says something **must not** occur. If it's violated,
you can point to the exact moment it went wrong.

| Example | Property | Violation |
|---------|----------|-----------|
| Stock never goes negative | `stock >= 0` always | stock = -1 at timestamp T |
| No money created from nothing | `sum(balances) = constant` | sum changed at T |
| No duplicate orders | Each order_id appears once | order_id X seen twice |
| Confirmed order always persisted | `confirmed → persisted` | Confirmed but no DB row |

**In Faultbox:** safety properties map to `assert_never()` and monitors
that call `fail()` on violation.

```python
# Safety: stock never negative
assert_never(where=lambda e:
    e.type == "stdout" and e.service == "inventory"
    and e.data.get("stock") is not None
    and int(e.data["stock"]) < 0)
```

### Liveness: "good things eventually happen"

A liveness property says something **must eventually** occur. If it's
violated, you only know after waiting long enough.

| Example | Property | Violation |
|---------|----------|-----------|
| Every order is eventually processed | Processing completes | Order stuck forever |
| Failed writes are eventually retried | Retry happens | No retry after 30s |
| Kafka message eventually consumed | Consumer processes it | Message sits unconsumed |
| Circuit breaker eventually closes | Recovery happens | Permanently open |

**In Faultbox:** liveness properties map to `assert_eventually()`.

```python
# Liveness: retry eventually happens
assert_eventually(where=lambda e:
    e.type == "stdout" and e.service == "api"
    and e.data.get("action") == "retry")
```

### Why both matter

Safety without liveness: the system never does anything wrong because it
never does anything at all. (A service that rejects all requests is "safe.")

Liveness without safety: the system eventually processes everything but
corrupts data along the way.

You need both. Safety says the system doesn't break things. Liveness says
the system actually works.

## Finding invariants in your system

### Technique 1: Follow the money

For any system that manages valuable state (money, orders, inventory,
subscriptions), trace the lifecycle:

```
Created → Processing → Committed → Confirmed to user
```

At each transition, ask:
- Can the state go backwards? (safety violation)
- Can it get stuck? (liveness violation)
- Can it skip a step? (safety violation)
- Can two transitions happen simultaneously? (concurrency bug)

### Technique 2: Read your error handling

Your existing error handling code implicitly defines invariants:

```go
if err := db.Insert(order); err != nil {
    return fmt.Errorf("order not persisted: %w", err)
}
kafka.Publish("order-created", order)
```

This code assumes: "if Insert fails, don't publish to Kafka." The
invariant is: **no Kafka event without a successful DB insert.** Express
it as a monitor on the fault assumption:

```python
def no_orphan_events(event):
    if event["type"] == "topic" and event.get("topic") == "order-created":
        db_rows = events(where=lambda e:
            e.type == "wal" and e.data.get("action") == "INSERT")
        if len(db_rows) == 0:
            fail("Kafka event published without DB insert")

orphan_check = monitor(no_orphan_events)

db_write_fail = fault_assumption("db_write_fail",
    target = db,
    write = deny("EIO"),
    monitors = [orphan_check],
)
```

### Technique 3: Production incidents

Every production incident reveals a violated invariant. After an incident:
1. What property was violated? (e.g., "orders were duplicated")
2. Write a Faultbox test that reproduces the failure mode
3. Add a monitor that catches the violation

This turns incidents into permanent protection.

### Technique 4: Ask "what if both?"

For any pair of operations, ask: "what if both happen at the same time?"

- Two orders for the last item → stock goes negative?
- Write + read simultaneously → read returns stale data?
- Retry + original succeed simultaneously → duplicate processing?

```python
def concurrent_orders():
    results = parallel(
        lambda: api.post(path="/orders", body='{"item":"widget","qty":25}'),
        lambda: api.post(path="/orders", body='{"item":"widget","qty":25}'),
    )
    return results

scenario(concurrent_orders)

# Use fault_scenario with an expect that checks the invariant:
fault_scenario("no_overselling",
    scenario = concurrent_orders,
    expect = lambda results: assert_true(
        len([r for r in results if r.status == 200]) <= 1,
        "oversold: both orders succeeded"),
)
```

## Expressing invariants in Faultbox

### assert_never — "this must not happen"

```python
# No denied syscall should go unhandled (the service should catch it)
assert_never(where=lambda e:
    e.type == "syscall" and e.decision.startswith("deny")
    and e.service == "api",
    msg="API received denied syscall — check error handling")
```

### assert_eventually — "this must happen"

```python
# After a successful order, the WAL must be written
assert_eventually(where=lambda e:
    e.type == "wal" and e.data.get("table") == "orders"
    and e.data.get("action") == "INSERT")
```

### assert_before — "this must happen in this order"

```python
# WAL write must happen before the HTTP response
assert_before(
    first={"service": "db", "syscall": "write", "path": "*.wal"},
    then={"service": "api", "type": "step_recv"},
)
```

### Combining: a complete invariant as expect oracle

```python
def create_order():
    return api.post(path="/orders", body='{"item":"widget","qty":1}')

scenario(create_order)

fault_scenario("order_durability",
    scenario = create_order,
    expect = lambda r: (
        assert_eq(r.status, 200),

        # Safety: the data was actually written
        assert_eventually(where=lambda e:
            e.type == "wal" and e.data.get("table") == "orders"),

        # Ordering: write happened before response
        assert_before(
            first={"service": "db", "type": "wal"},
            then={"service": "test", "type": "step_recv"},
        ),
    ),
)
```

## Invariants under fault — the domain-centric approach

The real test: do invariants hold when things break? In the domain model,
invariants travel with fault assumptions — you don't wire them per-test:

```python
# Invariant monitor: confirmed orders must be persisted
def order_persisted_check(event):
    if (event["type"] == "stdout" and event["service"] == "api"
            and event.get("status") == "confirmed"):
        rows = events(where=lambda e:
            e.type == "wal" and e.data.get("action") == "INSERT")
        if len(rows) == 0:
            fail("order confirmed but not persisted!")

persistence_monitor = monitor(order_persisted_check, service="api")

# Fault assumptions carry the invariant
db_slow = fault_assumption("db_slow",
    target = db,
    write = delay("500ms"),
    monitors = [persistence_monitor],
)

db_write_fail = fault_assumption("db_write_fail",
    target = db,
    write = deny("EIO"),
    monitors = [persistence_monitor],
)

# Matrix: every cell verifies the invariant automatically
fault_matrix(
    scenarios = [create_order],
    faults = [db_slow, db_write_fail],
    overrides = {
        (create_order, db_slow): lambda r: (
            assert_true(r.status == 200 or r.status >= 500),
        ),
        (create_order, db_write_fail): lambda r: (
            assert_true(r.status >= 500),
        ),
    },
)
```

Both matrix cells run `persistence_monitor` — if the API confirms an order
that isn't persisted, the monitor catches it regardless of which fault is
active.

## What you learned

- **Tests** verify one scenario; **invariants** verify a property across all scenarios
- **Safety properties:** bad things never happen (`assert_never`, monitors)
- **Liveness properties:** good things eventually happen (`assert_eventually`)
- **Domain-centric invariants:** monitors on `fault_assumption(monitors=)` fire across all matrix cells
- **Finding invariants:** follow the money, read error handling, learn from incidents
- **Expressing invariants:** monitors for real-time, `assert_*` in `expect` for post-hoc

## What's next

Chapter 15 introduces **monitors** in depth — the lifecycle, temporal
property patterns, and how first-class `MonitorDef` values compose with
fault assumptions and matrices.
