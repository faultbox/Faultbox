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
# If we told the user 'confirmed', the data MUST be in the DB.
monitor("order_confirmed_means_persisted",
    on = match.event(type="stdout", service="api"),
    check = lambda event, state:
        not (event.data.get("status") == "confirmed"
             and event.data.get("persisted") != "true"),
)
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

A monitor's `check=` lambda returns `False` to fail the test. Here it
fails the instant a stdout event reports negative stock:

```python
monitor("no_negative_stock",
    on    = match.event(type="stdout", service="inventory"),
    check = lambda event, state:
        event.data.get("stock") == None or int(event.data["stock"]) >= 0,
)
```

### Pattern 2: "If A happens, B must have happened"

A monitor keeps per-test state in `state=`. `update=` accumulates the
IDs seen on each side (both services log JSON to stdout, captured via
`observe.stdout`); `check=` verifies that every confirmed order is also
persisted. The `on=` matcher uses `match.any(...)` so the monitor sees
events from both services:

```python
monitor("order_confirmed_means_persisted",
    on = match.any(
        match.event(type="stdout", service="api"),
        match.event(type="stdout", service="db"),
    ),
    state_init = {"confirmed": [], "persisted": []},
    update = lambda event, state: {
        "confirmed": state["confirmed"] + (
            [event.data["order_id"]]
            if event.data.get("action") == "order_confirmed" else []),
        "persisted": state["persisted"] + (
            [event.data["order_id"]]
            if event.data.get("action") == "INSERT" else []),
    },
    # Every confirmed order must already appear in the persisted set.
    check = lambda event, state:
        all([oid in state["persisted"] for oid in state["confirmed"]]),
)

def test_order_flow():
    api.post(path="/orders", body='...')
    # The monitor above runs on every event and fails the test if an
    # order is ever confirmed without a matching persist.
```

Attach `observe=[observe.stdout(decoder=decoder("json"))]` to both the
`api` and `db` services so their log lines arrive as `type="stdout"`
events with structured fields in `event.data`.

### Pattern 3: "A must happen before B"

`assert_before(first=, then=)` asserts one event precedes another in the
trace. Here we require the DB to `fsync` its WAL before the API emits its
"responded" log line - i.e. data is durable before the client is told OK:

```python
def test_wal_before_response():
    """Data must be written to WAL before the API responds."""
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 201)

        assert_before(
            first={"service": "db", "syscall": "fsync"},
            then=lambda e: e.type == "stdout" and e.service == "api"
                and e.data.get("action") == "responded",
        )
    trace(db, syscalls=["write", "fsync"], run=scenario)
```

### Pattern 4: "Count constraint"

A monitor counts matching events in `state=` and fails if the count ever
exceeds the bound. Here: an order must produce **at most** one publish
log line (the producer logs each publish to stdout):

```python
monitor("at_most_one_publish",
    on = match.event(type="stdout", service="producer"),
    state_init = {"n": 0},
    update = lambda event, state: {
        "n": state["n"] + (1 if event.data.get("action") == "publish" else 0),
    },
    check = lambda event, state: state["n"] <= 1,
)

def test_exactly_one_event():
    """Each order should produce exactly one publish event."""
    api.post(path="/orders", body='...')
    # The monitor fails the moment a second publish is logged.
```

For an exact-count check (exactly one, not just "at most one"), assert on
the event count at the end of the scenario:

```python
def test_exactly_one_event():
    api.post(path="/orders", body='...')
    publishes = events(where=lambda e:
        e.type == "stdout" and e.service == "producer"
        and e.data.get("action") == "publish")
    assert_eq(len(publishes), 1,
        "expected 1 event, got " + str(len(publishes)))
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

**Best practice:** put invariant monitors in a shared file behind a
function that returns them, then attach them where you need them:

```python
# invariants.star
def invariant_monitors():
    return [
        monitor("no_negative_stock",
            on = match.event(type="stdout", service="inventory"),
            check = lambda event, state:
                event.data.get("stock") == None or int(event.data["stock"]) >= 0,
        ),
        monitor("no_orphan_events",
            on = match.event(type="stdout", service="worker"),
            check = lambda event, state:
                event.data.get("action") != "orphan",
        ),
    ]
```

Calling `invariant_monitors()` at the top level of a spec registers each
monitor **spec-wide** - it runs on every test:

```python
# any-test.star
load("invariants.star", "invariant_monitors")

INVARIANTS = invariant_monitors()   # spec-wide: active in every test

def test_my_scenario():
    # invariants are active during this test
    ...
```

To scope invariants to a particular fault instead, pass them to
`fault_assumption(monitors=)` / `fault_scenario(monitors=)` /
`fault_matrix(monitors=)`:

```python
db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
    monitors = invariant_monitors(),   # active whenever this fault applies
)
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
