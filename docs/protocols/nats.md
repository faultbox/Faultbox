# NATS Protocol Reference

Interface declaration:

```python
nats = service("nats",
    interface("main", "nats", 4222),
    image = "nats:2.10",
    healthcheck = tcp("localhost:4222"),
)
```

## Methods

### `publish(subject="", data="")`

Publish a message to a subject.

```python
nats.main.publish(subject="orders.new", data='{"id":1,"item":"widget"}')
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `subject` | string | NATS subject (e.g., `"orders.new"`, `"events.>"`) |
| `data` | string | Message payload |

**Response:** `{"published": true, "subject": "orders.new"}`

### `request(subject="", data="")`

Send a request and wait for a reply (request-reply pattern).

```python
resp = nats.main.request(subject="orders.get", data='{"id":1}')
# resp.data = {"subject": "orders.get", "data": "{\"id\":1,\"status\":\"confirmed\"}"}
```

### `subscribe(subject="")`

Subscribe and receive one message.

```python
resp = nats.main.subscribe(subject="orders.*")
# resp.data = {"subject": "orders.new", "data": "..."}
```

## Fault Rules

### `drop(topic=)`

Drop messages matching the subject pattern.

```python
message_loss = fault_assumption("message_loss",
    target = nats.main,
    rules = [drop(topic="orders.*")],
)
```

**Note:** The `topic` parameter matches NATS subjects (despite the name).

### `delay(topic=, delay=)`

```python
slow_nats = fault_assumption("slow_nats",
    target = nats.main,
    rules = [delay(topic="*", delay="2s")],
)
```

## Seed / Reset Patterns

NATS is stateless by default — messages are fire-and-forget. No seed or
reset needed unless using JetStream (persistent streams).

```python
nats = service("nats",
    interface("main", "nats", 4222),
    image = "nats:2.10",
    reuse = True,
    # No seed/reset — NATS is stateless
)
```
