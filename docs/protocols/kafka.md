# Kafka Protocol Reference

Interface declaration:

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    healthcheck = ready(timeout = "120s"),
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

### `consume(topic="", group=)`

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
| `group` | string | scoped to (run, test) | Consumer group ID |

> **The default group is per-run and per-test**, named
> `faultbox-<run>-<test>`. Pass `group=` only when the spec is *about*
> consumer-group semantics — rebalances, redelivery, offset commits — where
> a stable name is the point.
>
> A consumer group's committed offsets live in the broker's
> `__consumer_offsets` and outlive the reader that wrote them. Through
> v0.18.0 the default was the constant `"faultbox"`, so the second test to
> consume resumed after whatever the first had committed, and a broker
> container kept across tests (`reuse=True`) carried that state between
> whole runs. What a test saw depended on what had run before it, **at any
> seed** — the seed could not fix it, because the state is in the broker,
> not in Faultbox. A group that has never committed anything falls back to
> kafka-go's `FirstOffset`, so the read starts at the beginning of the
> topic every time.
>
> This makes a run **reproducible**; it does not isolate topic contents.
> On a reused broker a test still reads the *first* message on the topic,
> which may be an earlier test's. For isolation, use a per-test topic —
> see Option 1 below.

**Response fields:**

| Field | Type | Description |
|-------|------|-------------|
| `.data["topic"]` | string | Topic name |
| `.data["partition"]` | int | Partition number |
| `.data["offset"]` | int | Message offset |
| `.data["key"]` | string | Message key |
| `.data["value"]` | string | Message value |

## Fault Rules

> **Before you write one: point the broker at the proxy.**
>
> Kafka clients ask the broker where it is. The `Metadata` response carries
> `advertised.listeners`, and the client opens every later connection to
> **that** address — not to the one it bootstrapped against. So a fault rule
> sees the bootstrap exchange and nothing else: no produce, no fetch, no
> match, and a `FAULT_NOT_FIRED` warning that looks like a bad matcher.
>
> Make the broker advertise the proxy instead of itself. `proxy_addr` is
> late-bound, so this resolves after the proxy has a port:
>
> ```python
> kafka = service("kafka",
>     interface("main", "kafka", 9092),
>     image = "apache/kafka:3.7.0",
>     env = {
>         "KAFKA_ADVERTISED_LISTENERS": "PLAINTEXT://" + kafka.main.proxy_addr,
>         # …the rest of the KRaft configuration
>     },
> )
> ```
>
> This is single-broker only. A multi-broker cluster advertises one address
> per node and needs one proxy listener per node, which Faultbox does not do
> yet — see [RFC-057](../rfcs/0057-advertised-address-rewriting.md). The same
> limitation applies to Redis Cluster and MongoDB replica sets, which
> advertise addresses from runtime state rather than configuration and so
> have no equivalent workaround.

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

Duplicate messages — the consumer sees each message twice. The proxy
forwards the produce normally, then re-sends it once; the producer still
receives a single ack.

```python
duplicates = fault_assumption("duplicates",
    target = kafka.broker,
    rules = [duplicate(topic="order-events")],
)
```

> **Idempotent producers:** modern Kafka clients default to
> `enable.idempotence=true`, and a real broker deduplicates the re-sent
> batch (same producer id and sequence number) — the consumer will NOT
> see the message twice. `duplicate()` exercises the consumer's
> duplicate-handling against non-idempotent producers and mock brokers;
> to test it with an idempotent producer, disable idempotence for the
> test or produce the duplicate at the application level.

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

> **Not yet callable from Starlark.** `topic()` is a Go event-source
> plugin with no Starlark constructor wired yet, so a spec calling
> `topic(...)` fails to load (`undefined: topic`). Until it ships, observe
> the consumer's stdout log with
> [`observe.stdout`](../spec-language.md#event-sources) and query
> `type == "stdout"` events (see
> [tutorial ch11](../tutorial/05-advanced/11-event-sources.md)). The shape
> below describes the `topic` source once exposed.

Capture all messages on a topic in the event log:

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.6",
    observe = [topic("order-events", decoder=decoder("json"))],  # planned
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

A monitor's `update`/`check` lambdas run sandboxed - they cannot issue
queries or call `fail()`. Express the invariant instead by counting the
two observed event streams and checking the relation: a published order
must never outrun the committed rows. (Requires the `topic` and `wal`
observers above; both are planned Starlark surfaces - see the note under
[Topic observer](#topic-observer).)

```python
orphan_check = monitor("no_orphan_events",
    on = match.any(match.event(type="topic"), match.event(type="wal")),
    state_init = {"published": 0, "committed": 0},
    update = lambda event, state: {
        "published": state["published"] + (1 if event.type == "topic" else 0),
        "committed": state["committed"] + (
            1 if event.type == "wal" and event.data.get("op") == "INSERT" else 0),
    },
    # false => test FAILs, citing the offending event
    check = lambda event, state: state["published"] <= state["committed"],
)

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
