# Invariants & Monitors

How to find, encode, and verify the safety properties your system must
maintain — regardless of what goes wrong.

## What's an invariant?

A **test** checks one scenario: "when X happens, Y should be the result."

An **invariant** is stronger: "Y must ALWAYS be true, no matter what
happens." It's not tied to a specific test — it runs across all tests,
all faults, all interleavings.

### Example: the difference

**Test:** "When the DB is down, the API returns 503."

```python
def test_db_down():
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 503)
    fault(api, connect=deny("ECONNREFUSED"), run=scenario)
```

**Invariant:** "An order confirmed to the user is ALWAYS persisted in the database."

```python
def order_confirmed_means_persisted(event):
    """If we told the user 'confirmed', the data MUST be in the DB."""
    if (event.type == "stdout" and event.service == "api"
            and event.data.get("status") == "confirmed"
            and event.data.get("persisted") != "true"):
        fail("order confirmed but not persisted!")

monitor(order_confirmed_means_persisted, service="api")
```

The test verifies one scenario. The invariant catches a bug in ANY
scenario — including ones you haven't written yet.

## How to find invariants

### Ask: "What must never happen?"

For each service, ask: "what would be catastrophic if it happened?"

| System | Catastrophic event | Invariant |
|---|---|---|
| E-commerce | User charged, order not created | Payment → order must be atomic |
| Banking | Money created from nothing | Sum of all balances = constant |
| Inventory | Stock goes negative | `stock >= 0` always |
| Messaging | Duplicate messages delivered | Each message ID delivered at most once |
| Auth | Valid token rejected | Authenticated user always authorized |

### Ask: "What must always happen?"

| System | Required behavior | Invariant |
|---|---|---|
| Database | Committed data survives restart | WAL write before commit response |
| API | Failed request leaves no partial state | Rollback on error |
| Queue | Published message eventually delivered | No message loss |
| Cache | Cache consistent with source | Invalidation after write |

### Ask: "What ordering must hold?"

| System | Required ordering | Invariant |
|---|---|---|
| Payments | Charge before fulfill | Payment event before shipping event |
| Event sourcing | Event published after DB commit | WAL write before Kafka publish |
| Distributed lock | Lock acquired before critical section | Lock event before write event |

## Encoding invariants as monitors

### Pattern 1: "This should never happen"

```python
def no_negative_stock(event):
    if (event.type == "stdout" and event.service == "inventory"
            and event.data.get("stock") is not None):
        stock = int(event.data["stock"])
        if stock < 0:
            fail("stock went negative: " + str(stock))

monitor(no_negative_stock, service="inventory")
```

### Pattern 2: "If A happens, B must have happened"

```python
confirmed_orders = {"ids": []}
persisted_orders = {"ids": []}

def track_confirmed(event):
    if (event.type == "stdout" and event.service == "api"
            and event.data.get("action") == "order_confirmed"):
        confirmed_orders["ids"].append(event.data["order_id"])

def track_persisted(event):
    if (event.type == "wal" and event.data.get("table") == "orders"
            and event.data.get("action") == "INSERT"):
        persisted_orders["ids"].append(event.data["order_id"])

monitor(track_confirmed, service="api")
monitor(track_persisted, service="db")

def test_order_flow():
    api.post(path="/orders", body='...')

    # After the test, verify invariant:
    for oid in confirmed_orders["ids"]:
        assert_true(oid in persisted_orders["ids"],
            "order " + oid + " confirmed but not persisted")
```

### Pattern 3: "A must happen before B"

```python
def test_wal_before_response():
    """Data must be written to WAL before the API responds."""
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 201)

        assert_before(
            first={"service": "db", "type": "wal", "data.action": "INSERT"},
            then={"service": "api", "type": "step_recv"},
        )
    trace(db, syscalls=["write", "fsync"], run=scenario)
```

### Pattern 4: "Count constraint"

```python
publish_count = {"n": 0}

def count_publishes(event):
    if event.type == "topic" and event.data.get("topic") == "order-events":
        publish_count["n"] += 1

monitor(count_publishes)

def test_exactly_one_event():
    """Each order should produce exactly one Kafka event."""
    publish_count["n"] = 0
    api.post(path="/orders", body='...')
    assert_eq(publish_count["n"], 1,
        "expected 1 event, got " + str(publish_count["n"]))
```

## Monitors vs assertions

| | Assertions | Monitors |
|---|---|---|
| **When** | After a step or at test end | On every event, in real-time |
| **Scope** | One test | Every test (when registered) |
| **Catches** | Expected failures | Unexpected violations |
| **Example** | `assert_eq(resp.status, 503)` | "stock never negative" |

**Use assertions** for: "this specific thing should happen in this test."

**Use monitors** for: "this property should ALWAYS hold."

**Best practice:** put invariant monitors in a shared file and load them
in every spec:

```python
# invariants.star
def register_invariants():
    monitor(no_negative_stock, service="inventory")
    monitor(no_orphan_events, service="worker")
    monitor(no_duplicate_messages)
```

```python
# any-test.star
load("invariants.star", "register_invariants")
register_invariants()

def test_my_scenario():
    # invariants are active during this test
    ...
```

## Invariants under fault

The real power: invariants that hold EVEN WHEN things break.

```python
def test_db_down_preserves_invariants():
    """When the DB is down, invariants still hold:
    - No data is lost (nothing was committed)
    - No false confirmation (API returns error, not 200)
    - No partial state (no orphaned Kafka events)
    """
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_true(resp.status >= 400)
    fault(db, connect=deny("ECONNREFUSED"), run=scenario)
    # Monitors (no_negative_stock, no_orphan_events) run automatically
```

If the invariant monitor fails during a fault test, you've found a real
bug: the system violates a safety property under failure.

## Progression

1. **Start with one invariant** — the most important one for your system
2. **Add it as a monitor** to your existing happy-path tests
3. **Run fault tests with the monitor active** — does it catch anything?
4. **Add more invariants** as you discover them (production incidents are a good source)
5. **Put them in a shared file** so every new test gets them automatically
