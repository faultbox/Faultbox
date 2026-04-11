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
# Data written to DB must not be lost
def test_write_persisted():
    resp = api.http.post(path="/users", body='{"name":"alice"}')
    assert_eq(resp.status, 201)
    assert_eventually(where=lambda e:
        e.type == "wal" and e.data.get("action") == "INSERT")

# Cache failure must not cause data loss
monitor(lambda e:
    fail("data loss") if e.type == "stdout" and "lost" in e.data.get("msg", ""),
    service="api",
)
```

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
    observe=[topic("order-events", decoder=json_decoder())],
)

db = service("db",
    interface("pg", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test"},
    healthcheck=tcp("localhost:5432"),
    observe=[wal_stream(tables=["orders"])],
)

producer = service("producer",
    interface("http", "http", 8080),
    image="myapp-producer:latest",
    env={"KAFKA_BROKERS": kafka.broker.internal_addr},
    depends_on=[kafka],
    healthcheck=http("localhost:8080/health"),
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
        # The event should not appear in the consumer's DB
        assert_never(where=lambda e:
            e.type == "wal" and e.data.get("table") == "orders")
    fault(kafka.broker, drop(topic="order-events"), run=scenario)
```

### Key invariants

```python
# Every published event must eventually be consumed and persisted
published = {"ids": []}
persisted = {"ids": []}

def track_published(event):
    if event.type == "topic" and event.data.get("topic") == "order-events":
        published["ids"].append(event.data.get("order_id"))

def track_persisted(event):
    if event.type == "wal" and event.data.get("table") == "orders":
        persisted["ids"].append(event.data.get("order_id"))

monitor(track_published)
monitor(track_persisted, service="db")

# No duplicate messages
seen_ids = {"set": set()}

def no_duplicates(event):
    if event.type == "topic" and event.data.get("order_id"):
        oid = event.data["order_id"]
        if oid in seen_ids["set"]:
            fail("duplicate message: " + oid)
        seen_ids["set"].add(oid)

monitor(no_duplicates)
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
# (inventory reserved, payment charged)
def no_partial_orders(event):
    if (event.type == "stdout" and event.service == "gateway"
            and event.data.get("status") == "200"
            and event.data.get("order_complete") != "true"):
        fail("gateway returned 200 but order not complete")

monitor(no_partial_orders, service="gateway")

# Response time SLA: no request should take > 10s
def sla_check(event):
    if event.type == "step_recv" and event.service == "test":
        duration = int(event.fields.get("duration_ms", "0"))
        if duration > 10000:
            fail("SLA violation: " + str(duration) + "ms")

monitor(sla_check)
```

## Adapting patterns to your project

1. **Identify which pattern is closest** to your architecture
2. **Copy the topology** and adjust service names, images, ports
3. **Pick 3-5 critical faults** from the pattern's list
4. **Add 1-2 invariants** — the most important ones for your business
5. **Run and iterate** — the first run will reveal missing error handling
