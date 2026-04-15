# Chapter 8: Database & Broker Protocol Faults

**Duration:** 25 minutes
**Prerequisites:** [Chapter 7 (HTTP Protocol Faults)](07-http-redis.md) completed

> **This chapter uses containers.** The examples below use `image=` to run
> real Postgres, Redis, and Kafka instances in Docker containers. If you
> haven't used containers with Faultbox before, see
> [Chapter 9: Containers](../04-advanced/09-containers.md) for setup
> instructions and how `image=` mode works. Docker must be available in
> your environment (it's pre-installed in the Lima VM).

## Goals & Purpose

HTTP faults are useful for API gateways, but the most critical failures
happen at the **database and message broker** level:

- "What if this INSERT query returns an error?"
- "What if Kafka drops a message on the orders topic?"
- "What if Redis returns READONLY for SET commands?"
- "What if a gRPC call returns UNAVAILABLE?"

The domain-centric model separates **what you do** (scenarios) from
**what breaks** (fault assumptions) and **what you expect** (assertions).
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

# --- Scenarios: probe functions that return observables ---

def create_user():
    return api.post(path="/users", body='{"name":"alice"}')
scenario(create_user)

def get_user():
    return api.get(path="/users/1")
scenario(get_user)

# --- Fault assumptions: named, reusable fault configs ---

insert_error = fault_assumption("insert_error",
    target = db.main,
    error(query="INSERT*", message="disk full"),
)

slow_select = fault_assumption("slow_select",
    target = db.main,
    delay(query="SELECT*", delay="3s"),
)

# --- Fault scenarios: composed tests ---

fault_scenario("insert_fails",
    scenario = create_user,
    faults = insert_error,
    expect = lambda r: assert_true(r.status >= 400,
        "expected error when INSERT fails, got " + str(r.status)),
)

fault_scenario("slow_query",
    scenario = get_user,
    faults = slow_select,
    expect = lambda r: assert_true(r.duration_ms > 2000, "expected slow response"),
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

def create_user_mysql():
    return api.post(path="/users", body='{"name":"alice"}')
scenario(create_user_mysql)

mysql_readonly = fault_assumption("mysql_readonly",
    target = db.main,
    error(query="INSERT*", message="Table is read only"),
)

fault_scenario("mysql_read_only",
    scenario = create_user_mysql,
    faults = mysql_readonly,
    expect = lambda r: assert_true(r.status >= 400),
)
```

Same fault assumption API — the proxy knows to speak MySQL wire protocol
because the interface is declared as `"mysql"`.

## Redis: Command-level faults

```python
cache = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
)

# --- Scenarios ---

def write_cache():
    return api.post(path="/data/key", body="value")
scenario(write_cache)

def read_cache():
    return api.get(path="/data/key")
scenario(read_cache)

# --- Fault assumptions ---

redis_readonly = fault_assumption("redis_readonly",
    target = cache.main,
    error(command="SET", key="*", message="READONLY"),
)

cache_miss = fault_assumption("cache_miss",
    target = cache.main,
    response(command="GET", key="*"),  # empty body = nil response
)

slow_cache = fault_assumption("slow_cache",
    target = cache.main,
    delay(command="GET", delay="2s"),
)

# --- Fault scenarios ---

fault_scenario("redis_readonly",
    scenario = write_cache,
    faults = redis_readonly,
    expect = lambda r: assert_true(r.status >= 500, "expected error on READONLY Redis"),
)

fault_scenario("cache_miss",
    scenario = read_cache,
    faults = cache_miss,
    expect = lambda r: assert_eq(r.status, 200, "should serve from DB on cache miss"),
)

fault_scenario("slow_cache",
    scenario = read_cache,
    faults = slow_cache,
    expect = lambda r: assert_true(r.duration_ms < 3000, "should timeout, not hang"),
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

# --- Scenarios ---

def publish_orders():
    for i in range(10):
        api.post(path="/orders", body='{"id":' + str(i) + '}')
scenario(publish_orders)

def publish_single_order():
    api.post(path="/orders", body='{"id":1}')
scenario(publish_single_order)

# --- Fault assumptions ---

message_drop = fault_assumption("message_drop",
    target = kafka.main,
    drop(topic="orders.events", probability="30%"),
)

slow_delivery = fault_assumption("slow_delivery",
    target = kafka.main,
    delay(topic="orders.events", delay="5s"),
)

# --- Fault scenarios ---

fault_scenario("message_drop",
    scenario = publish_orders,
    faults = message_drop,
    # Worker should process most but handle missing ones.
)

fault_scenario("slow_delivery",
    scenario = publish_single_order,
    faults = slow_delivery,
    # Worker eventually processes, but with delay.
)
```

## gRPC: Method-level faults

```python
auth_svc = service("auth",
    interface("grpc", "grpc", 9090),
    image = "company/auth:latest",
)

# --- Scenarios ---

def login():
    return api.post(path="/login", body='{"user":"alice"}')
scenario(login)

# --- Fault assumptions ---

auth_unavailable = fault_assumption("auth_unavailable",
    target = auth_svc.grpc,
    error(method="/auth.AuthService/Authenticate", status=14),  # UNAVAILABLE
)

slow_auth = fault_assumption("slow_auth",
    target = auth_svc.grpc,
    delay(method="/auth.AuthService/*", delay="5s"),
)

# --- Fault scenarios ---

fault_scenario("auth_unavailable",
    scenario = login,
    faults = auth_unavailable,
    expect = lambda r: assert_eq(r.status, 503),
)

fault_scenario("slow_auth",
    scenario = login,
    faults = slow_auth,
    expect = lambda r: assert_true(r.duration_ms > 4000),
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

- `scenario(fn)` registers probe functions that return observables
- `fault_assumption(name, target=, ...)` defines named, reusable fault configs
- `fault_scenario(name, scenario=, faults=, expect=)` composes tests from scenarios and faults
- The proxy speaks the real wire protocol — matches SQL queries, Redis commands, Kafka topics
- 10 protocols supported, covering 30+ products
- Combine with syscall faults for cascading failure scenarios

## What's next

Part 4 covers advanced features: testing real infrastructure with Docker
containers, auto-generating failure scenarios from happy paths, capturing
structured events from stdout and message queues, and defining high-level
named operations.
