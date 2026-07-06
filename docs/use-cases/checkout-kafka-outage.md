# Use Case: How a Checkout Survives a Kafka Outage

**Shape:** worked example - an order service, a Postgres it owns, and a
Kafka it publishes to. Three bug classes closed in one spec: unhandled
dependency failure, missing timeout, failed recovery.

This is the incident every event-driven checkout has either had or is
about to have: the broker goes away for five minutes, and the
interesting question is not "did requests fail?" but "did money and
state stay consistent, and did the system actually come back?"

## The topology

```python
db = service("db", image = "postgres:16-alpine",
    interface("main", "postgres", 5432),
    volumes = {"./schema.sql": "/docker-entrypoint-initdb.d/schema.sql"},
    reuse   = True,
    reset   = lambda: db.main.exec(sql = "TRUNCATE orders RESTART IDENTITY CASCADE"),
)

kafka = service("kafka", image = "bitnami/kafka:3.7",
    interface("main", "kafka", 9092),
)

api = service("api", image = "myteam/checkout-api",
    interface("public", "http", 8080),
    env = {"DB_ADDR": db.main.addr, "KAFKA_ADDR": kafka.main.addr},
    depends_on = [db, kafka],
    healthcheck = http("localhost:8080/health"),
)
```

Only `myteam/checkout-api` is yours. Postgres and Kafka are stock
images - and if you can't run a real Kafka in CI, the same spec works
against a mock broker.

## Class 1: the broker is gone

What should checkout do when Kafka is down - fail the purchase, or
accept it and publish later? Whatever your answer is, write it down:

```python
kafka_down = fault_assumption("kafka_down",
    target  = kafka,
    connect = deny("ECONNREFUSED"),
)

fault_scenario("checkout_broker_down",
    scenario = place_order,
    faults   = kafka_down,
    expect   = lambda r: (
        assert_eq(r.status, 201, "checkout must accept the order"),
        assert_eq(r.json["fulfillment"], "deferred"),
    ),
)
```

The wrong answers this catches: a 500 to the customer because the
producer constructor panicked, or - worse - a 201 with the order row
written and the event silently dropped on the floor.

## Class 2: the broker is slow, not down

Slow is more dangerous than down: nothing errors, everything queues.

```python
kafka_slow = fault_assumption("kafka_slow",
    target = kafka.main,
    rules  = [delay(topic = "orders", delay = "2s")],
)

fault_scenario("checkout_broker_slow",
    scenario = place_order,
    faults   = kafka_slow,
    expect   = lambda r: (
        assert_eq(r.status, 201),
        assert_lt(r.elapsed, "500ms",
            "publish must be async or deadlined - checkout latency must not track broker latency"),
    ),
)
```

## Class 5: the broker comes back - does the system?

The least-tested transition in production systems. The fault clears;
the deferred events must actually flow.

```python
def test_broker_recovery(t):
    with fault(kafka, connect = deny("ECONNREFUSED")):
        api.public.post(path = "/orders", body = '{"cart": 42}')
    # fault cleared - recovery is now the thing under test:
    eventually(lambda trace: trace.event(type = "kafka_publish", topic = "orders"),
               within = "30s")
```

If the producer is stuck on a dead connection pool, this test renders
FAIL - not a silent green, and not a pass that quietly never checked
anything: if the run ends before the evidence arrives, the verdict is
INCONCLUSIVE.

## The invariant that ties it together

Orders and events must reconcile no matter which fault fired:

```python
def no_lost_orders(trace):
    placed    = trace.count(match.event(type = "order_created"))
    published = trace.count(match.event(type = "kafka_publish", topic = "orders"))
    return published >= placed  # deferred is fine; lost is not

always(no_lost_orders)
```

## What this buys you

Three specs and one invariant close the three ways this outage hurts:
the customer-facing failure, the latency coupling, and the silent
non-recovery. Total spec size: under 80 lines. The `.fb` bundle from a
failing run attaches to the incident ticket and replays the exact fault
schedule that produced it.
