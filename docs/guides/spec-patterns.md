# Spec Patterns for Real Projects

Cookbook patterns for common architectures. Each pattern shows the
topology, critical faults to test, key invariants, and a starter spec.

## Pattern 1: API + Database + Cache

The most common pattern. An HTTP API backed by Postgres with Redis caching.

```
Client → API (HTTP :8080) → Postgres (5432)
                          → Redis (6379)
```

### Topology

```python
db = service("db",
    interface("pg", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "mydb"},
    healthcheck=tcp("localhost:5432"),
    observe=[observe.stdout(decoder=decoder("json"))],
)

cache = service("cache",
    interface("redis", "redis", 6379),
    image="redis:7",
    healthcheck=tcp("localhost:6379"),
)

api = service("api",
    interface("http", "http", 8080),
    image="myapp:latest",
    env={
        "DATABASE_URL": "postgres://test@" + db.pg.internal_addr + "/mydb",
        "REDIS_URL": "redis://" + cache.redis.internal_addr,
    },
    depends_on=[db, cache],
    healthcheck=http("localhost:8080/health"),
    observe=[observe.stdout(decoder=decoder("json"))],
)
```

### Critical faults

```python
# 1. Database down — does the API return useful errors?
def test_db_down():
    def scenario():
        resp = api.http.post(path="/users", body='{"name":"alice"}')
        assert_eq(resp.status, 503)
    fault(api, connect=deny("ECONNREFUSED", label="db down"), run=scenario)

# 2. Cache down — does the API fall back to DB?
def test_cache_down():
    def scenario():
        resp = api.http.get(path="/users/1")
        assert_eq(resp.status, 200, "should work without cache")
    fault(api, connect=deny("ECONNREFUSED", label="cache down"), run=scenario)

# 3. DB disk full — does the API handle write failures?
def test_db_disk_full():
    def scenario():
        resp = api.http.post(path="/users", body='{"name":"bob"}')
        assert_true(resp.status >= 500)
    fault(db, write=deny("ENOSPC", label="disk full"), run=scenario)

# 4. Slow DB — does the API timeout or hang?
def test_slow_db():
    def scenario():
        resp = api.http.get(path="/users/1")
        assert_true(resp.duration_ms < 5000, "should timeout, not hang")
    fault(db, write=delay("3s"), run=scenario)

# 5. Cache stale — does the API serve stale data?
def test_specific_query_fails():
    def scenario():
        resp = api.http.post(path="/users", body='{"name":"charlie"}')
        assert_true(resp.status >= 500)
    fault(db.pg, error(query="INSERT INTO users*"), run=scenario)
```

### Key invariants

```python
# Data written to DB must not be lost. The DB logs each commit to
# stdout (captured via observe.stdout); assert one arrived.
def test_write_persisted():
    resp = api.http.post(path="/users", body='{"name":"alice"}')
    assert_eq(resp.status, 201)
    assert_eventually(where=lambda e:
        e.type == "stdout" and e.data.get("action") == "INSERT")

# Cache failure must not cause data loss: fail if the API ever logs
# a "lost" message. check= returning False fails the test.
monitor("no_data_loss",
    on    = match.event(type="stdout", service="api"),
    check = lambda event, state: "lost" not in event.data.get("msg", ""),
)
```

Attach `observe=[observe.stdout(decoder=decoder("json"))]` to `db` and
`api` so their structured log lines become `type="stdout"` events.

## Pattern 2: Event-Driven (Producer + Broker + Consumer)

Services communicate through a message broker.

```
Producer (HTTP :8080) → Kafka (9092) → Consumer (HTTP :8081)
                                     → Database (5432)
```

### Topology

```python
kafka = service("kafka",
    interface("broker", "kafka", 9092),
    image="confluentinc/cp-kafka:7.6",
    healthcheck=tcp("localhost:9092"),
)

db = service("db",
    interface("pg", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test"},
    healthcheck=tcp("localhost:5432"),
    observe=[observe.stdout(decoder=decoder("json"))],
)

producer = service("producer",
    interface("http", "http", 8080),
    image="myapp-producer:latest",
    env={"KAFKA_BROKERS": kafka.broker.internal_addr},
    depends_on=[kafka],
    healthcheck=http("localhost:8080/health"),
    observe=[observe.stdout(decoder=decoder("json"))],
)

consumer = service("consumer",
    interface("http", "http", 8081),
    image="myapp-consumer:latest",
    env={
        "KAFKA_BROKERS": kafka.broker.internal_addr,
        "DATABASE_URL": "postgres://test@" + db.pg.internal_addr + "/mydb",
    },
    depends_on=[kafka, db],
    healthcheck=http("localhost:8081/health"),
    observe=[observe.stdout(decoder=decoder("json"))],
)
```

### Critical faults

```python
# 1. Kafka down — does the producer return error or silently drop?
def test_kafka_down():
    def scenario():
        resp = producer.http.post(path="/orders", body='{"item":"widget"}')
        assert_true(resp.status >= 500, "should fail, not silently drop")
    fault(producer, connect=deny("ECONNREFUSED", label="kafka down"), run=scenario)

# 2. Consumer DB down — does the consumer retry or lose the message?
def test_consumer_db_down():
    def scenario():
        producer.http.post(path="/orders", body='{"item":"widget"}')
        # Message should NOT be acknowledged if DB write fails
        # (it should be retried later)
    fault(db, write=deny("EIO", label="db down"), run=scenario)

# 3. Message loss — Kafka drops a message
def test_kafka_message_drop():
    def scenario():
        producer.http.post(path="/orders", body='{"item":"widget"}')
        # The event should not appear in the consumer's DB - the
        # consumer logs each persisted row to stdout.
        assert_never(where=lambda e:
            e.type == "stdout" and e.service == "consumer"
            and e.data.get("action") == "INSERT")
    fault(kafka.broker, drop(topic="order-events"), run=scenario)
```

### Key invariants

```python
# Every published event must eventually be consumed and persisted.
# The producer logs each publish and the DB logs each persist to
# stdout; one monitor tracks both sides in state= and fails if any
# published order_id is missing from the persisted set.
monitor("published_is_persisted",
    on = match.any(
        match.event(type="stdout", service="producer"),
        match.event(type="stdout", service="db"),
    ),
    state_init = {"published": [], "persisted": []},
    update = lambda event, state: {
        "published": state["published"] + (
            [event.data["order_id"]]
            if event.data.get("action") == "publish" else []),
        "persisted": state["persisted"] + (
            [event.data["order_id"]]
            if event.data.get("action") == "INSERT" else []),
    },
    check = lambda event, state:
        all([oid in state["persisted"] for oid in state["published"]]),
)

# No duplicate messages - track seen order_ids and fail on a repeat.
monitor("no_duplicate_messages",
    on = match.event(type="stdout", service="producer"),
    state_init = {"seen": []},
    update = lambda event, state: {
        "seen": state["seen"] + (
            [event.data["order_id"]]
            if event.data.get("action") == "publish" else []),
    },
    # The just-published id must not already have been seen before this event.
    check = lambda event, state:
        event.data.get("action") != "publish"
        or state["seen"].count(event.data["order_id"]) <= 1,
)
```

## Pattern 3: Microservice Mesh

Multiple services calling each other over HTTP/gRPC.

```
Gateway (HTTP :8080) → Orders (gRPC :50051) → Inventory (gRPC :50052)
                     → Payments (gRPC :50053)
                     → Notifications (gRPC :50054)
```

### Topology

```python
inventory = service("inventory",
    interface("grpc", "grpc", 50052),
    image="myapp-inventory:latest",
    healthcheck=tcp("localhost:50052"),
)

payments = service("payments",
    interface("grpc", "grpc", 50053),
    image="myapp-payments:latest",
    healthcheck=tcp("localhost:50053"),
)

orders = service("orders",
    interface("grpc", "grpc", 50051),
    image="myapp-orders:latest",
    env={
        "INVENTORY_ADDR": inventory.grpc.internal_addr,
        "PAYMENTS_ADDR": payments.grpc.internal_addr,
    },
    depends_on=[inventory, payments],
    healthcheck=tcp("localhost:50051"),
)

gateway = service("gateway",
    interface("http", "http", 8080),
    image="myapp-gateway:latest",
    env={"ORDERS_ADDR": orders.grpc.internal_addr},
    depends_on=[orders],
    healthcheck=http("localhost:8080/health"),
    observe=[observe.stdout(decoder=decoder("json"))],
)
```

### Critical faults

```python
# 1. One downstream is down — does the gateway degrade gracefully?
def test_payments_down():
    def scenario():
        resp = gateway.http.post(path="/orders", body='...')
        # Should fail clearly, not timeout or return partial data
        assert_true(resp.status in [503, 502])
        assert_true(resp.duration_ms < 5000, "should not hang")
    fault(orders, connect=deny("ECONNREFUSED", label="payments down"), run=scenario)

# 2. Inventory slow — does the order timeout?
def test_inventory_slow():
    def scenario():
        resp = gateway.http.post(path="/orders", body='...')
        assert_true(resp.duration_ms < 10000, "must timeout")
    fault(inventory, write=delay("8s"), run=scenario)

# 3. Network partition between orders and inventory
def test_partition_orders_inventory():
    def scenario():
        resp = gateway.http.post(path="/orders", body='...')
        assert_true(resp.status >= 500)
    partition(orders, inventory, run=scenario)

# 4. Cascade: payments down + inventory slow
def test_cascade():
    def scenario():
        resp = gateway.http.post(path="/orders", body='...')
        assert_true(resp.status >= 500)
        assert_true(resp.duration_ms < 5000, "should fail fast")
    
    def with_slow_inventory():
        fault(inventory, write=delay("3s"), run=scenario)
    
    fault(orders, connect=deny("ECONNREFUSED", label="payments down"),
        run=with_slow_inventory)
```

### Key invariants

```python
# If the gateway returns 200, the order must be fully processed
# (inventory reserved, payment charged). The gateway logs each
# completed request to stdout; fail if a 200 lacks order_complete.
monitor("no_partial_orders",
    on = match.event(type="stdout", service="gateway"),
    check = lambda event, state:
        not (event.data.get("status") == "200"
             and event.data.get("order_complete") != "true"),
)

# Response time SLA: no logged request should report > 10s.
monitor("sla_check",
    on = match.event(type="stdout", service="gateway"),
    check = lambda event, state:
        int(event.data.get("duration_ms", "0")) <= 10000,
)
```

## Adapting patterns to your project

1. **Identify which pattern is closest** to your architecture
2. **Copy the topology** and adjust service names, images, ports
3. **Pick 3-5 critical faults** from the pattern's list
4. **Add 1-2 invariants** — the most important ones for your business
5. **Run and iterate** — the first run will reveal missing error handling
