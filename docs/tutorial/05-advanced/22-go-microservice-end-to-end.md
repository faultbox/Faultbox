# Chapter 22: End-to-End — A Real Go Microservice

**Duration:** 30 minutes
**Prerequisites:** Comfortable with prior chapters (containers, mocks,
gRPC, JWT, fault_matrix). This chapter ties them together.

## Goals & purpose

You have a Go microservice. It talks to MySQL, Redis, Kafka, three
internal gRPC upstreams, validates JWTs, and runs behind an HTTP
load balancer. Production has had four incidents in six months —
two database flakes, one cascading auth failure, one Kafka rebalance
that knocked out half the consumer fleet.

This chapter builds, end-to-end, the Faultbox spec that would have
caught all four. By the end of 30 minutes you'll have:

- Stack composed of real containers (MySQL, Redis, Kafka),
  protocol-aware mocks (gRPC upstreams via descriptors, JWT issuer
  via `jwt.server`), and your SUT binary.
- Smoke tests that prove the stack boots green.
- A `fault_matrix` covering 4 scenarios × 6 fault classes (24 cells)
  with explicit `expect_*` outcomes per cell.
- A reproducible `.fb` bundle uploaded as a CI artifact.

The shape mirrors what inDrive's truck-api PoC produced over six
weeks; this is the curated short version.

## Stack overview

The service we'll model:

```
┌─────────┐
│ Client  │── Bearer JWT ──▶ ┌──────────┐
│ (test)  │                  │  api     │── MySQL (orders/offers)
└─────────┘                  │  (Go)    │── Redis (cache)
                             │          │── Kafka (event bus)
                             │          │── gRPC: geo-config
                             │          │── gRPC: user-service
                             │          │── gRPC: balance-api
                             └──────────┘
                                  ▲
                                  └── JWKS fetched from auth issuer
```

Three categories of dependency:

- **Real containers** for stateful infra (MySQL, Redis, Kafka).
  These have nuanced syscall and protocol behaviour we want to
  exercise faithfully.
- **Typed gRPC mocks** for internal upstreams (geo-config etc.).
  Real instances would need their own configs, secrets, and a
  sidecar fleet — unworkable for a hermetic test.
- **JWT issuer mock** for the auth layer. Real OIDC issuers are
  out-of-scope to operate from a test env.

## 1 · Topology

`faultbox.star`:

```python
load("@faultbox/mocks/jwt.star",  "jwt")
load("@faultbox/mocks/grpc.star", "grpc")

# --- Stateful real services (containers) -------------------------

db = service("db", image = "mysql:8.0.32",
    interface("main", "mysql", 3306),
    env = {
        "MYSQL_ROOT_PASSWORD": "test",
        "MYSQL_DATABASE":      "appdb",
    },
    seed = lambda: mysql.exec(db.main, sql = load_file("./schema.sql")),
    healthcheck = tcp("localhost:3306"),
    reuse = True,  # 5x faster across multi-test runs
)

cache = service("cache", image = "redis:7-alpine",
    interface("main", "redis", 6379),
    healthcheck = tcp("localhost:6379"),
    reuse = True,
)

bus = service("bus", image = "apache/kafka:3.7.0",
    interface("main", "kafka", 9092),
    healthcheck = kafka_ready("localhost:9092"),
    reuse = True,
)

# --- Internal gRPC upstreams (typed mocks) -----------------------

geo = grpc.server(
    name        = "geo-config",
    interface   = interface("main", "grpc", 9001),
    descriptors = "./proto/all_upstreams.pb",
    services    = {
        "/inDriver.geo_config.GeoConfigService/GetCity": {
            "response": {"id": 169, "name": "Almaty", "country": "KZ"},
        },
    },
)

users = grpc.server(
    name        = "user-service",
    interface   = interface("main", "grpc", 9002),
    descriptors = "./proto/all_upstreams.pb",
    services    = {
        "/inDriver.users.UserService/Get": {
            "response": {"id": 1, "name": "alice", "verified": True},
        },
    },
)

balance = grpc.server(
    name        = "balance-api",
    interface   = interface("main", "grpc", 9003),
    descriptors = "./proto/all_upstreams.pb",
    services    = {
        "/inDriver.balance.BalanceService/Get": {
            "response": {"user_id": 1, "amount": "5000.00"},
        },
    },
)

# --- Auth issuer (JWT mock) --------------------------------------

auth = jwt.server(
    name      = "auth",
    interface = interface("main", "http", 8090),
    issuer    = "https://faultbox-auth.invalid",
)

# --- The SUT -----------------------------------------------------

api = service("api", binary = "./bin/api",
    interface("public", "http", 8080),
    env = {
        "DATABASE_URL":      "mysql://root:test@%s/appdb" % db.main.addr,
        "REDIS_URL":         cache.main.addr,
        "KAFKA_BROKERS":     bus.main.addr,
        "GEO_CONFIG_ADDR":   geo.service.main.addr,
        "USER_SERVICE_ADDR": users.service.main.addr,
        "BALANCE_API_ADDR":  balance.service.main.addr,
        "OIDC_ISSUER":       auth.service.main.addr,
        "OIDC_JWKS_URL":     auth.service.main.addr + "/.well-known/jwks.json",
    },
    depends_on = [db, cache, bus, geo.service, users.service, balance.service, auth.service],
    healthcheck = http("http://localhost:8080/healthz", expect_status = 200),
)
```

Notable choices:

- **`reuse=True` on stateful containers** brings test iteration time
  down from ~25s/test to ~10s/test. Spec authors call `seed=` once
  per cold start; subsequent tests reuse the running container.
- **`load_file("./schema.sql")`** instead of code-generating a 1000-
  line string constant — landed in v0.9.8.
- **`jwt.server()`** + **`grpc.server(descriptors=…)`** replace the
  hand-written Go binaries every customer ended up writing.
- **All upstream addresses go through the data-path proxy** (RFC-024
  shipped v0.9.5) automatically — no spec changes needed for that.

## 2 · Smoke tests

Three tests prove the stack boots green before you spend time on
fault scenarios:

```python
def test_health_endpoint():
    result = step(api.public, "get", path = "/healthz")
    assert_true(result.status_code == 200)

def test_jwt_protected_endpoint():
    token = auth.sign(claims = {"sub": "user-1", "scope": "read:orders"})
    result = step(api.public, "get",
                  path = "/orders",
                  headers = {"Authorization": "Bearer " + token})
    assert_true(result.status_code == 200)

def test_create_order():
    token = auth.sign(claims = {"sub": "user-1", "scope": "write:orders"})
    result = step(api.public, "post",
                  path = "/orders",
                  headers = {"Authorization": "Bearer " + token},
                  body = '{"price":5000,"city_id":169}')
    assert_true(result.status_code == 201)
```

Run with `faultbox test faultbox.star --test test_health_endpoint`
to verify each in isolation. All three should be green before
moving on.

## 3 · Reusable scenarios

Wrap the smoke tests as scenarios so the fault_matrix can drive
them under chaos:

```python
def order_creation():
    token = auth.sign(claims = {"sub": "user-1", "scope": "write:orders"})
    return step(api.public, "post",
                path = "/orders",
                headers = {"Authorization": "Bearer " + token},
                body = '{"price":5000,"city_id":169}')

def order_listing():
    token = auth.sign(claims = {"sub": "user-1", "scope": "read:orders"})
    return step(api.public, "get",
                path = "/orders",
                headers = {"Authorization": "Bearer " + token})

def health_probe():
    return step(api.public, "get", path = "/healthz")

def login_flow():
    token = auth.sign(claims = {"sub": "user-1"})
    return step(api.public, "post",
                path = "/auth/refresh",
                headers = {"Authorization": "Bearer " + token})
```

Each scenario returns the `step()` result — outcomes get fed to
`expect_*` predicates per matrix cell.

## 4 · Fault assumptions

Six fault classes, mirroring real production failure modes:

```python
db_down = fault_assumption("db_down",
    target = db.main,
    rules  = [error(status_code = 0, drop = True)],  # connection refused
)

db_slow = fault_assumption("db_slow",
    target = db.main,
    rules  = [response(delay_ms = 3000)],
)

db_write_fail = fault_assumption("db_write_fail",
    target = db,
    write  = deny("EIO"),
)

cache_down = fault_assumption("cache_down",
    target  = cache,
    connect = deny("ECONNREFUSED"),
)

bus_down = fault_assumption("bus_down",
    target  = bus,
    connect = deny("ECONNREFUSED"),
)

users_unavailable = fault_assumption("users_unavailable",
    target = users.service.main,
    rules  = [grpc.unavailable("user-service down")],
)
```

Mix of:

- **Protocol-level proxy faults** (`db_down`, `db_slow`,
  `users_unavailable`) — exercise client-side retry/timeout/circuit-
  breaker behaviour.
- **Syscall-level faults** (`db_write_fail`, `cache_down`, `bus_down`)
  — exercise low-level error paths the protocol proxy can't reach
  (e.g., write-after-connect failures).

## 5 · The matrix

```python
fault_matrix(
    scenarios      = [order_creation, order_listing, health_probe, login_flow],
    faults         = [db_down, db_slow, db_write_fail, cache_down, bus_down, users_unavailable],
    default_expect = expect_success(),
    overrides = {
        # Order creation HAS to write to DB. db_down/write_fail → 5xx
        # within timeout budget.
        (order_creation, db_down):       expect_error_within(ms = 5000),
        (order_creation, db_write_fail): expect_error_within(ms = 5000),
        # Listing reads from cache first, falls back to DB.
        # cache_down should still succeed (DB read), db_down with
        # cold cache → 5xx.
        (order_listing, db_down):        expect_error_within(ms = 5000),
        # Health endpoint must stay green under every infra fault.
        # If it doesn't, the LB will mark the pod unhealthy in prod
        # and we just made the outage worse.
        (health_probe, db_down):         expect_success(),
        (health_probe, db_slow):         expect_success(),
        (health_probe, cache_down):      expect_success(),
        (health_probe, bus_down):        expect_success(),
        (health_probe, db_write_fail):   expect_success(),
        # Login pulls from user-service. Without it, expect 401-ish
        # within the auth-middleware timeout budget.
        (login_flow, users_unavailable): expect_error_within(ms = 8000),
    },
)
```

That's a 4 × 6 = 24-cell matrix, each with an explicit outcome.
The HTML report (RFC-029, v0.11.0) renders this as a green/red/yellow
grid; for now every cell pass/fails individually with descriptive
errors when an `expect_*` is violated.

## 6 · Run it

```sh
$ faultbox test faultbox.star --seed 1
Seed: 1 (cli)

--- PASS: test_health_endpoint (12ms, seed=1) ---
--- PASS: test_jwt_protected_endpoint (28ms, seed=1) ---
--- PASS: test_create_order (45ms, seed=1) ---
--- PASS: matrix_order_creation_db_down (203ms, seed=1) ---
--- FAIL: matrix_order_creation_cache_down (12ms, seed=1) ---
  reason: expect_success: status_code=503 (want < 500)
... 22 more cells ...

Bundle: run-2026-04-23T11-12-34-1.fb

22 passed, 2 failed

Replay: faultbox replay run-2026-04-23T11-12-34-1.fb --test matrix_order_creation_cache_down --seed 1
```

A cell that violates `expect_success()` likely means: cache failure
unexpectedly cascaded to a 5xx instead of falling back to the DB
path. That's the kind of bug fault-injection finds — exactly the
point.

## 7 · CI integration

Drop the [GitHub Actions template](../../guides/ci-on-linux.md) into
your repo. The `.fb` bundle uploads as a build artifact on failure;
your reviewer downloads it and runs `faultbox inspect` locally to
triage without rebuilding the env.

## What we built

| Piece | Lines of Starlark | What it does |
|---|---|---|
| Topology | ~50 | 4 services, 4 mocks, env wiring |
| Smoke tests | ~15 | Pre-fault sanity |
| Scenarios | ~15 | Reusable behaviours |
| Fault assumptions | ~30 | 6 protocol/syscall fault classes |
| Matrix | ~20 | 24 cells with explicit expectations |
| **Total** | **~130** | Catches the four prod incident classes |

Compare to what the same coverage looked like before v0.9.x:
hand-written Go binary stubs (~500 lines), code-generated SQL
constant (~1500 lines), no JWKS mock at all (separate Go service).
v0.9.x is the difference between "it'd be nice to test resilience"
and "we test resilience on every PR."

## See also

- [bundles.md](../../bundles.md) — `.fb` artifact reference,
  upload to CI, replay locally.
- [ci-on-linux.md](../../guides/ci-on-linux.md) — Actions/BuildKite
  templates with privilege requirements documented.
- [troubleshooting.md](../../troubleshooting.md) — when something
  in this stack misbehaves on your machine.
- [seccomp-cheatsheet.md](../../seccomp-cheatsheet.md) — picking
  the right syscall for `db_write_fail` style rules.
