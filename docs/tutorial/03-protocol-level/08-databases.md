# Chapter 8: Database & Broker Protocol Faults

**Duration:** 25 minutes
**Prerequisites:** [Chapter 7 (HTTP & Redis Faults)](07-http-redis.md) completed

## Goals & Purpose

HTTP faults are useful for API gateways, but the most critical failures
happen at the **database and message broker** level:

- "What if this INSERT query returns an error?"
- "What if Kafka drops a message on the orders topic?"
- "What if Redis returns READONLY for SET commands?"
- "What if a gRPC call returns UNAVAILABLE?"

The same `fault(interface_ref, ...)` mechanism works for all protocols.
Each proxy speaks the real wire protocol and can inject protocol-specific
errors.

## Postgres: Query-level faults

```python
db = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {"DATABASE_URL": "postgres://test@" + db.main.internal_addr + "/testdb"},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def test_insert_fails():
    """INSERT queries return an error — API should handle gracefully."""
    def scenario():
        resp = api.post(path="/users", body='{"name":"alice"}')
        assert_true(resp.status >= 400,
            "expected error when INSERT fails, got " + str(resp.status))
    fault(db.main,
        error(query="INSERT*", message="disk full"),
        run=scenario,
    )

def test_slow_query():
    """SELECT queries are delayed 3s — API should timeout."""
    def scenario():
        resp = api.get(path="/users/1")
        assert_true(resp.duration_ms > 2000, "expected slow response")
    fault(db.main,
        delay(query="SELECT*", delay="3s"),
        run=scenario,
    )
```

The proxy parses the Postgres wire protocol — it sees the actual SQL query
text and matches against your pattern.

## MySQL: Same API, different protocol

```python
db = service("mysql",
    interface("main", "mysql", 3306),
    image = "mysql:8",
    env = {"MYSQL_ROOT_PASSWORD": "test"},
    healthcheck = tcp("localhost:3306"),
)

def test_mysql_read_only():
    """All INSERT queries fail — simulating a read-only replica."""
    def scenario():
        resp = api.post(path="/users", body='{"name":"alice"}')
        assert_true(resp.status >= 400)
    fault(db.main,
        error(query="INSERT*", message="Table is read only"),
        run=scenario,
    )
```

Same `fault(db.main, error(query=..., message=...))` — the proxy knows
to speak MySQL wire protocol because the interface is declared as `"mysql"`.

## Redis: Command-level faults

```python
cache = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
)

def test_redis_readonly():
    """SET commands return READONLY error — simulating a replica."""
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_true(resp.status >= 500, "expected error on READONLY Redis")
    fault(cache.main,
        error(command="SET", key="*", message="READONLY"),
        run=scenario,
    )

def test_cache_miss():
    """GET commands return nil — simulating empty cache."""
    def scenario():
        resp = api.get(path="/data/key")
        # API should fall back to database.
        assert_eq(resp.status, 200, "should serve from DB on cache miss")
    fault(cache.main,
        response(command="GET", key="*"),  # empty body = nil response
        run=scenario,
    )

def test_slow_cache():
    """Redis GET is delayed 2s — API should timeout and use DB."""
    def scenario():
        resp = api.get(path="/data/key")
        assert_true(resp.duration_ms < 3000, "should timeout, not hang")
    fault(cache.main,
        delay(command="GET", delay="2s"),
        run=scenario,
    )
```

## Kafka: Message-level faults

```python
kafka = service("kafka",
    interface("main", "kafka", 9092),
    image = "confluentinc/cp-kafka:7.5.0",
)

worker = service("worker",
    interface("public", "http", 8081),
    build = "./worker",
    env = {"BROKER": kafka.main.internal_addr},
    depends_on = [kafka],
)

def test_message_drop():
    """30% of Kafka messages are dropped — worker should handle gaps."""
    def scenario():
        # Publish 10 messages.
        for i in range(10):
            api.post(path="/orders", body='{"id":' + str(i) + '}')
        # Worker should process most but handle missing ones.
    fault(kafka.main,
        drop(topic="orders.events", probability="30%"),
        run=scenario,
    )

def test_slow_delivery():
    """Kafka messages are delayed 5s — worker should handle lag."""
    def scenario():
        api.post(path="/orders", body='{"id":1}')
        # Worker eventually processes, but with delay.
    fault(kafka.main,
        delay(topic="orders.events", delay="5s"),
        run=scenario,
    )
```

## gRPC: Method-level faults

```python
auth_svc = service("auth",
    interface("grpc", "grpc", 9090),
    image = "company/auth:latest",
)

def test_auth_unavailable():
    """Auth service returns UNAVAILABLE — API should return 503."""
    def scenario():
        resp = api.post(path="/login", body='{"user":"alice"}')
        assert_eq(resp.status, 503)
    fault(auth_svc.grpc,
        error(method="/auth.AuthService/Authenticate", status=14),  # UNAVAILABLE
        run=scenario,
    )

def test_slow_auth():
    """Auth RPC is delayed — API should timeout."""
    def scenario():
        resp = api.post(path="/login", body='{"user":"alice"}')
        assert_true(resp.duration_ms > 4000)
    fault(auth_svc.grpc,
        delay(method="/auth.AuthService/*", delay="5s"),
        run=scenario,
    )
```

## Protocol fault reference

| Protocol | Match by | Fault types |
|----------|----------|-------------|
| **HTTP** | `method=`, `path=` | response, error, delay, drop |
| **Postgres** | `query=` | error, delay, drop |
| **MySQL** | `query=` | error, delay, drop |
| **Redis** | `command=`, `key=` | error, response (nil), delay, drop |
| **Kafka** | `topic=` | drop, delay, error |
| **gRPC** | `method=` | error (status code), delay, drop |
| **MongoDB** | `method=` (command), `key=` (collection) | error, delay, drop |
| **AMQP** | `topic=` (routing key) | drop, delay, error |
| **NATS** | `topic=` (subject) | drop, delay |
| **Memcached** | `command=`, `key=` | error, response, delay, drop |

## What you learned

- Same `fault(interface_ref, ...)` API for all protocols
- The proxy speaks the real wire protocol — matches SQL queries, Redis commands, Kafka topics
- 10 protocols supported, covering 30+ products
- Combine with syscall faults for cascading failure scenarios

## What's next

Part 4 covers advanced features: testing real infrastructure with Docker
containers, auto-generating failure scenarios from happy paths, capturing
structured events from stdout and message queues, and defining high-level
named operations.
