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

Publish a message to a topic and wait for broker acknowledgement (`acks=all`).
Each step owns its transport, so restarting a mock at the same address does
not reuse connections or metadata from an earlier test.

```python
kafka.broker.publish(topic="order-events", data='{"id":1,"action":"created"}', key="order-1")
kafka.broker.publish(topic="notifications", data="hello world")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `topic` | string | required | Topic name |
| `data` | string or bytes | `""` | Message value (body), transmitted unchanged |
| `key` | string or bytes | `""` | Message key (for partitioning) |

For protobuf messages, use the shared encoder:

```python
payload = proto_encode(
    descriptors = "proto/events.pb",
    message = "orders.v1.OrderCreated",
    body = {"order_id": "123", "city_id": 1},
)
kafka.broker.publish(topic="orders", data=payload, key=b"\x00\xff")
```

`proto_encode()` returns Starlark `bytes` and validates message/field names
using protobuf JSON rules. Its descriptor file is included in bundles.

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
| `.data["key_base64"]` | string | Exact key bytes as base64 |
| `.data["value_base64"]` | string | Exact value bytes as base64 (use for binary payloads) |

### Bounded batch observation: `consume_many(...)`

```python
batch = bus.main.consume_many(
    topic="users.v1",
    max_records=100,
    timeout="5s",
    idle_timeout="250ms",
    descriptors="proto/users.pb",
    message="example.User",
)
assert_true(batch.ok, batch.error)
for record in batch.data["records"]:
    user = record["data"]
    print(user["id"], record["partition"], record["offset"])
```

| Argument | Default | Meaning |
|---|---|---|
| `topic` | required | Topic to observe |
| `max_records` | 100 | Maximum returned records, from 1 to 10000 |
| `timeout` | `"5s"` | Overall read/commit budget, including group assignment |
| `idle_timeout` | `"250ms"` | Quiet period after assignment or the last collected batch |
| `group` | scoped to run/test/plan leaf | Observation group; repeat calls resume after committed records |
| `descriptors`, `message` | omitted | Optional descriptor file and message FQN; provide both for typed decoding |

One client serves the entire call. It commits each fetched batch only after
all its records decode successfully, then closes at the end of the call.
It never defaults to the SUT's group. The descriptor file is captured for
bundle replay. The active test's cancellation interrupts the read; consumer
shutdown uses a bounded LeaveGroup wait.

The response contains `records`, `stop_reason`, `group`, `assigned` and
`committed_records`. Normal stop reasons are `max_records`, `idle_timeout`
and `timeout`. A quiet, assigned empty topic returns `ok=True` with an empty
list. Failure to get an assignment before timeout, broker/fetch errors,
malformed protobuf, failed commits and test cancellation return `ok=False`.
Partial records remain available; inspect `committed_records` after a failure.
A timed/idle observation does not prove that a live topic has no future records.

Each record carries `topic`, `partition`, exact integer `offset`,
`key_base64`, `value_base64`, `key_is_null`, and `value_is_null`. Readable
UTF-8 values also appear in `key`/`value`; those fields are null for binary
data or tombstones. Base64 plus the null flags preserve the exact wire value,
including the difference between an empty value and a tombstone.

Typed `data` follows protobuf JSON with proto field names, enums as names,
and int64/uint64 fields as strings. Tombstones have `data=None`; empty non-null
messages decode normally. Record order is guaranteed only within a partition.

For an existing single-record `consume()` result, or a saved binary payload:

```python
decoded = proto_decode(descriptors="proto/users.pb", message="example.User",
                       data_base64=record["value_base64"])
# Alternatively: data=wire_bytes (or a byte-carrying load_file() string).
```

`proto_decode` requires exactly one of `data` and `data_base64`. It rejects
invalid base64, invalid descriptors/message names and malformed wire data.
The existing single-record `consume()` API remains available.

## Fault Rules

Built-in single-broker Kafka mocks advertise their proxy automatically in
Metadata and FindCoordinator replies. When the topology includes containers,
the default advertised host is the host's `docker0` IPv4 address when available.
Custom networking can set `kafka.broker(advertise_host="...")` to an address
reachable by all clients. The setup below still applies to real brokers.


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

### Mock consumer-group readiness

Built-in Kafka mocks emit the following `mock.kafka.*` events. These describe
acknowledged **classic consumer-group protocol** exchanges (JoinGroup / SyncGroup /
Fetch), including Kafka-go, Sarama and the default franz-go group protocol.

| Event suffix | Evidence |
|---|---|
| `group_join` | Successful JoinGroup: `group`, `member_id`, `client_id`, `generation`, `epoch` |
| `assign` | Successful SyncGroup, one event per assigned `topic` / `partition`; adds `assignment` |
| `group_sync` | Completed SyncGroup, including empty assignments; adds `assignment`, `partitions` count |
| `fetch_position` | Successful Fetch at concrete `offset`, including empty responses; carries `client_id`, `topic`, `partition`, `request_sequence` |
| `group_ready` | Current assignment's first accepted Fetch position; adds `offset`, `attribution`, `position_source="fetch"` |
| `group_rebalance` | Prior readiness invalidated: a join was requested, a heartbeat failed, or a member left; adds `reason` and advances `epoch` |
| `group_leave` | Acknowledged departure of `member_id` |
| `group_ready_ambiguous` | More than one current assignment remains possible after process ownership and client/topic/partition matching |
| `group_attribution_unavailable` | Assignment or Fetch ownership is unresolved while the other is a known managed process; no readiness is inferred |

All numeric fields in these events are strings; convert with `int()`. `epoch`
and `assignment` are monotonic observation tokens scoped to this mock instance,
not Kafka offsets. A `group_rebalance` event is an invalidation signal and does
not claim that the requested rebalance succeeded.

**Wait for `group_ready` before publishing the first test record.** An assignment
alone is too early: a consumer with reset-to-latest may resolve the end offset
later and skip records published in between. Readiness waits for a concrete
successful Fetch, after offset reset, even when the topic is empty. No warm-up
record, committed offset or SUT log is needed. Incremental Fetch sessions retain
the acknowledged partition positions across empty responses.

Readiness is emitted per assigned partition, once per assignment. Reject stale
readiness after a rebalance; do not use an `any()` over all historical ready
events. For example, this predicate checks partition zero of a single topic:

```python
def consumer_positioned(group, topic, partition=0, source_service=None):
    history = events(service=bus.name, where=lambda e:
        e.type.startswith("mock.kafka.") and e.fields.get("group") == group)
    epoch = max([int(e.fields.get("epoch", "0")) for e in history] or [0])
    current = [e for e in history if int(e.fields.get("epoch", "0")) == epoch]
    generation = max([int(e.fields.get("generation", "0")) for e in current] or [0])
    current = [e for e in current if int(e.fields.get("generation", "0")) == generation]
    for ready in current:
        if ready.type != "mock.kafka.group_ready":
            continue
        f = ready.fields
        if source_service != None and f.get("source_service") != source_service:
            continue
        if f.get("topic") != topic or int(f.get("partition", "-1")) != partition:
            continue
        syncs = [int(e.fields["assignment"]) for e in current
                 if e.type == "mock.kafka.group_sync"
                 and e.fields.get("member_id") == f["member_id"]]
        if syncs and int(f["assignment"]) == max(syncs):
            return True
    return False

# In the test body, before its first publish:
for _ in range(100):
    if consumer_positioned("courier-group", "orders"):
        break
    sleep("100ms")
assert_true(consumer_positioned("courier-group", "orders"), "consumer not positioned")
bus.main.publish(topic="orders", data=payload)
```

For a multi-partition topic, check every partition the test may publish to.
Readiness is a startup barrier, not a continuous liveness or processing guarantee.
Use acknowledged commits past the published offset as a processing gate **only
if the SUT commits after processing**; Kafka also allows committing earlier.

Classic Fetch contains no group or member ID. On supported Linux managed
processes, Faultbox associates the actual socket with the registered service's
**process instance**, across separate coordinator and data connections. Kafka
`client_id` remains part of the match; service names and client IDs alone never
establish ownership. Two services can use the same client ID, topic and partition
in different groups: each becomes ready only after its own acknowledged Fetch.
Native Linux services register their host PID before the target is allowed to
exec, including launches without seccomp filtering. Ownership is established
from exact TCP endpoints and socket inodes within registered process trees;
proxy connections preserve that original ownership in memory without changing
Kafka headers or the SUT's `client_id`. `source_pid` is the registered service
root's host PID; verified descendants belong to the same service instance.
Kernel process start time and a registration token prevent PID reuse from
reviving an old identity. Container PIDs can be registered when available, but
network translation or inaccessible procfs can leave ownership unresolved.

These events carry `source_service`, `source_instance`, and `source_pid`, with
`attribution="process_instance"` on proven readiness. The event's `service`
continues to identify the broker mock. Source fields also accompany acknowledged
`produce`, `fetch`, and `commit` events, so completion gates can distinguish the
process that committed a record from another consumer with the same client ID.
When selecting by client ID/topic instead of a known group, also require the
intended `source_service`; identical IDs may identify two different consumers.
A completion gate should use the `group` and `source_instance` captured from
that consumer's ready event, plus the topic/partition and an offset past the
publish receipt. The other process's commit must not satisfy that gate.
For a restarted service, compare the current `source_instance`, not only its name
or PID; neither an old reply nor a fetch-session cache can position a new instance.

Ownership is conservative. Assignments with unresolved owners still count as
possible matches, and overlapping groups in the **same** process remain ambiguous.
An unresolved Fetch cannot borrow a managed member's readiness, even if its client
ID happens to be unique; `group_attribution_unavailable` explains that condition.
For external or unsupported processes where **both** assignment and Fetch are
unowned, the existing unique `(client_id, topic, partition)` fallback remains and
reports `attribution="unique_client_id"`. Source fields are omitted when unknown.
Give unowned clients distinct client IDs when their assignments overlap. A
manually assigned external reader must also use a distinct ID from a grouped
consumer of the same partitions. Raw `fetch_position` evidence remains available.
The mock observes wire exchanges; it does not independently announce silent
session expiry, nor support the KIP-848 consumer-group protocol in these events.

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


A successful `publish()` receipt includes `published`, `topic`, `partition`,
`offset`, `key_base64`, and `key_is_null`, taken from the acknowledged broker
response. A consumer commit must advance **past** that offset to acknowledge the
record; whether the commit occurs before or after processing is SUT-specific.
