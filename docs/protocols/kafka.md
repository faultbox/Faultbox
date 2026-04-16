# Kafka Protocol Reference

Interface declaration:

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    healthcheck = tcp("localhost:9092"),
)
```

## Methods

### `publish(topic="", data="", key="")`

Publish a message to a topic.

```python
kafka.broker.publish(topic="order-events", data='{"id":1,"action":"created"}', key="order-1")
kafka.broker.publish(topic="notifications", data="hello world")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `topic` | string | required | Topic name |
| `data` | string | required | Message value (body) |
| `key` | string | `""` | Message key (for partitioning) |

**Response:**

```python
resp = kafka.broker.publish(topic="events", data="test")
# resp.data = {"published": true, "topic": "events"}
```

### `consume(topic="", group="faultbox")`

Consume one message from a topic.

```python
resp = kafka.broker.consume(topic="order-events")
# resp.data = {
#   "topic": "order-events",
#   "partition": 0,
#   "offset": 42,
#   "key": "order-1",
#   "value": "{\"id\":1,\"action\":\"created\"}"
# }
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `topic` | string | required | Topic to consume from |
| `group` | string | `"faultbox"` | Consumer group ID |

**Response fields:**

| Field | Type | Description |
|-------|------|-------------|
| `.data["topic"]` | string | Topic name |
| `.data["partition"]` | int | Partition number |
| `.data["offset"]` | int | Message offset |
| `.data["key"]` | string | Message key |
| `.data["value"]` | string | Message value |

## Fault Rules

### `drop(topic=)`

Drop messages matching the topic — the producer thinks it published but
the message is lost.

```python
message_loss = fault_assumption("message_loss",
    target = kafka.broker,
    rules = [drop(topic="order-events")],
)
```

### `delay(topic=, delay=)`

Delay message delivery.

```python
slow_broker = fault_assumption("slow_broker",
    target = kafka.broker,
    rules = [delay(topic="*", delay="3s")],
)
```

### `duplicate(topic=)`

Duplicate messages — the consumer sees each message twice.

```python
duplicates = fault_assumption("duplicates",
    target = kafka.broker,
    rules = [duplicate(topic="order-events")],
)
```

## Seed / Reset Patterns

Kafka topics are append-only — you can't truncate them. Reset strategies:

```python
# Option 1: Use unique topic names per test run (no reset needed)
import time
TOPIC = "orders-" + str(int(time.time()))

# Option 2: Use consumer group offsets (consume from latest)
def reset_kafka():
    # Publish a marker, then consume until you see it
    kafka.broker.publish(topic="orders", data='{"marker":"reset"}')

# Option 3: Don't reuse Kafka (default — recreate between tests)
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    # reuse=False (default) — topic state resets with container
)
```

**Tip:** For most fault tests, `reuse=False` (default) is simplest —
each test gets a fresh Kafka with empty topics.

## Event Sources

### Topic observer

Capture all messages on a topic in the event log:

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    observe = [topic("order-events", decoder=json_decoder())],
)
```

Topic events have type `"topic"` with fields:

| Field | Type | Description |
|-------|------|-------------|
| `topic` | string | Topic name |
| `partition` | int | Partition number |
| `key` | string | Message key |
| `value` | string | Raw message value |
| `data` | dict | Auto-decoded JSON (if decoder set) |

```python
# Check a message was published
assert_eventually(where=lambda e:
    e.type == "topic" and e.data.get("topic") == "order-events"
    and e.data.get("action") == "created")

# Check NO message was published (on error)
assert_never(where=lambda e:
    e.type == "topic" and e.data.get("topic") == "order-events")
```

## Data Integrity Patterns

### No orphan events (publish without DB commit)

```python
def no_orphan_events(event):
    if event["type"] == "topic" and event.get("order_id"):
        rows = db.main.query(
            sql="SELECT count(*) as n FROM orders WHERE id='" + event["order_id"] + "'"
        ).data[0]["n"]
        if rows == 0:
            fail("orphan Kafka event: order " + event["order_id"] + " not in DB")

orphan_check = monitor(no_orphan_events)

db_write_error = fault_assumption("db_write_error",
    target = db,
    write = deny("EIO"),
    monitors = [orphan_check],
)
```

### No message loss

```python
fault_scenario("no_message_loss",
    scenario = publish_and_consume,
    faults = consumer_slow,
    expect = lambda r: assert_eq(
        len(events(where=lambda e: e.type == "topic" and e.data.get("action") == "produce")),
        len(events(where=lambda e: e.type == "topic" and e.data.get("action") == "consume")),
        "every produced message must be consumed"),
)
```

### Exactly-once delivery

```python
fault_scenario("no_duplicates",
    scenario = publish_order,
    faults = broker_restart,
    expect = lambda r: (
        # Count unique order IDs in consumed messages
        assert_eq(
            len(events(where=lambda e: e.type == "topic" and e.data.get("topic") == "order-events")),
            1,
            "exactly one message for this order"),
    ),
)
```

## Note on multi-process containers

Confluent Kafka images (`cp-kafka`, `cp-zookeeper`) use shell entrypoints
that fork Java. Faultbox automatically falls back to no-seccomp mode for
these — **syscall-level faults don't work**, but protocol-level faults
(via `rules=`) and event sources (via `observe=`) work normally.

```python
# This WORKS (protocol-level, via proxy):
message_loss = fault_assumption("message_loss",
    target = kafka.broker,
    rules = [drop(topic="orders")],
)

# This does NOT work on Confluent images (syscall-level, needs seccomp):
# disk_error = fault_assumption("disk_error", target=kafka, write=deny("EIO"))
```
