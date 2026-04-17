# RFC-015: Container Reuse with Seed/Reset Lifecycle

- **Status:** Implemented (v0.6.0)
- **Author:** Boris Glebov, Claude Opus 4.6
- **Created:** 2026-04-16
- **Branch:** `rfc/015-container-reuse`
- **Addresses:** FB-015, FB-012

## Summary

Add `reuse=True`, `seed=`, and `reset=` parameters to `service()` for
container lifecycle management. Containers with `reuse=True` are created
once, seeded once, and reset between tests — cutting multi-test execution
time by 5-10x.

## Motivation

### The performance problem

Each test creates all containers from scratch:

```
Test 1: pull images → create → healthcheck → run test → destroy   (20s)
Test 2: pull images → create → healthcheck → run test → destroy   (20s)
...
Test 11: same                                                      (20s)
Total: 11 × 20s = 220s (~4 minutes)
```

With a fault matrix of 71 cells: `71 × 20s = 24 minutes`.

### The state problem

Even without reuse, users need to initialize service state before tests.
Today the workaround is:

```python
# Hack: mount init.sql via volumes
postgres = service("postgres",
    image = "postgres:16",
    volumes = {"./init.sql": "/docker-entrypoint-initdb.d/init.sql"},
)
```

This only works for first start. It doesn't handle:
- Re-seeding between tests (data leaks from test 1 to test 2)
- Dynamic seed data (generated IDs, timestamps)
- Non-SQL services (Redis, Kafka, API warm-up calls)
- Seed data that depends on service being fully running (not just schema)

### The lifecycle gap

The real abstraction is **service lifecycle**: initialize state, run tests,
re-initialize between tests. Faultbox has none of this today.

## Design

### New parameters on `service()`

```
service(name, ...,
    reuse: bool = False,           # keep container alive between tests
    seed:  callable = None,        # initialize service state (after healthcheck)
    reset: callable = None,        # re-initialize between tests (if omitted, seed is called)
)
```

### Lifecycle

**Without `reuse` (current behavior, unchanged):**

```
Test 1: create → healthcheck → seed() → run test → destroy
Test 2: create → healthcheck → seed() → run test → destroy
```

**With `reuse=True`:**

```
Suite start: create → healthcheck → seed()
Test 1: run test
             ↓ reset() (or seed() if reset not set)
Test 2: run test
             ↓ reset()
Test 3: run test
Suite end: destroy
```

### Seed and reset callbacks

Callbacks are zero-argument Starlark callables. They run in the test
runtime context — all protocol steps, builtins, and service references
are available:

```python
def seed_db():
    postgres.main.exec(sql="CREATE TABLE IF NOT EXISTS orders (id SERIAL, item TEXT, qty INT)")
    postgres.main.exec(sql="CREATE TABLE IF NOT EXISTS users (id SERIAL, name TEXT)")
    postgres.main.exec(sql="INSERT INTO users (name) VALUES ('alice'), ('bob')")

def reset_db():
    postgres.main.exec(sql="TRUNCATE orders, users RESTART IDENTITY CASCADE")

postgres = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432"),
    reuse = True,
    seed = seed_db,
    reset = reset_db,
)
```

### When seed is defined but reset is not

If `seed` is set but `reset` is not, `seed` is called between tests.
This is the simple case — same function initializes and re-initializes:

```python
def setup_redis():
    redis.main.command("FLUSHALL")
    redis.main.set(key="config:max_retries", value="3")
    redis.main.set(key="config:timeout", value="5000")

redis = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
    reuse = True,
    seed = setup_redis,  # called once at start AND between tests
)
```

### When reset is cheaper than seed

For databases with expensive seed (migrations, large data sets), provide
a lightweight reset that truncates without re-migrating:

```python
def seed_full():
    """Run migrations + seed test data. Slow (~5s)."""
    postgres.main.exec(sql=open("./migrations.sql").read())
    postgres.main.exec(sql=open("./seed.sql").read())

def reset_fast():
    """Truncate data tables, keep schema. Fast (~100ms)."""
    postgres.main.exec(sql="TRUNCATE orders, payments, inventory RESTART IDENTITY CASCADE")

postgres = service("postgres", ...,
    reuse = True,
    seed = seed_full,     # once at suite start
    reset = reset_fast,   # between tests
)
```

### Stateless services — reuse without seed/reset

Services that don't accumulate state just need `reuse=True`:

```python
api = service("api",
    build = "./api",
    reuse = True,
    # No seed or reset — stateless HTTP service
)
```

### Services that can't be reused

Some services shouldn't be reused — Kafka with topic offsets, services
that write to local disk, services with connection pools that go stale.
These keep the default `reuse=False`:

```python
kafka = service("kafka",
    image = "confluentinc/cp-kafka:7.6",
    # reuse=False (default) — topic offsets carry between tests
)
```

### Interaction with fault injection

Fault rules are per-test. When a reused container runs test N under a
fault, the fault is removed before test N+1. The `reset` callback runs
AFTER fault removal, so the service is in a clean state (no faults active)
during reset.

```
Test 1: apply faults → run test → remove faults → reset()
Test 2: apply faults → run test → remove faults → reset()
```

### Interaction with fault_matrix

`fault_matrix` generates many cells. With reuse, all cells share the same
containers — only the fault rules and scenario change per cell:

```python
fault_matrix(
    scenarios = [create_order, list_orders, health_check],
    faults = [db_down, disk_full, slow_network],
)
# Without reuse: 9 cells × 20s = 3 minutes
# With reuse:    20s startup + 9 × 0.5s = 25 seconds
```

### Warning for missing reset

If `reuse=True` is set without `seed` or `reset`, emit a warning:

```
WARNING: service "postgres" has reuse=True but no seed/reset — state may leak between tests
```

This catches the common mistake of enabling reuse without thinking about
isolation.

## Full Example

```python
# --- Topology with lifecycle ---

postgres = service("postgres",
    interface("main", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432"),
    reuse = True,
    seed = lambda: postgres.main.exec(sql=open("./schema.sql").read()),
    reset = lambda: postgres.main.exec(sql="TRUNCATE orders, users RESTART IDENTITY CASCADE"),
)

redis = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7",
    healthcheck = tcp("localhost:6379"),
    reuse = True,
    seed = lambda: redis.main.command("FLUSHALL"),
)

api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {"DB": postgres.main.internal_addr, "REDIS": redis.main.internal_addr},
    depends_on = [postgres, redis],
    healthcheck = http("localhost:8080/health"),
    reuse = True,
)

# --- Scenarios ---

def create_order():
    return api.public.post(path="/orders", body='{"item":"widget","qty":1}')

scenario(create_order)

def list_orders():
    return api.public.get(path="/orders")

scenario(list_orders)

# --- Fault Assumptions ---

db_down = fault_assumption("db_down", target=api, connect=deny("ECONNREFUSED"))
disk_full = fault_assumption("disk_full", target=postgres, write=deny("ENOSPC"))

# --- Matrix ---

fault_matrix(
    scenarios = [create_order, list_orders],
    faults = [db_down, disk_full],
    overrides = {
        (create_order, db_down): lambda r: assert_eq(r.status, 503),
    },
)
# 4 cells, ~22s total instead of ~80s
```

## Implementation Plan

### Step 1: Parse reuse/seed/reset on service()

- Add `Reuse bool`, `Seed starlark.Callable`, `Reset starlark.Callable`
  fields to `ServiceDef`
- Parse in `builtinService()`
- Validate: `reset` without `reuse` is a warning (no effect)

### Step 2: Container lifecycle in Runtime

- Track reused containers separately: `rt.reusedContainers map[string]bool`
- In `startServices()`: skip create/healthcheck for already-running reused containers
- In `stopServices()`: skip destroy for reused containers (keep alive)
- New `rt.stopReusedContainers()`: called at suite end (after all tests)

### Step 3: Seed execution

- After `startServices()` and healthcheck pass, call `seed()` for each
  service that has it — but only on first start (not on reused resume)
- Seed runs in Starlark context: protocol steps available

### Step 4: Reset execution

- Before each test (except the first), call `reset()` (or `seed()` if
  no reset) for reused services
- Reset runs after fault removal, before next test's fault application
- Reset runs in dependency order (reset DB before API)

### Step 5: Warning for missing lifecycle

- Emit warning if `reuse=True` without `seed` or `reset`
- Emit diagnostic in JSON output if state leak suspected

## Open Questions

1. **Should `seed` run before or after the healthcheck?**
   Proposed: after. The service must be fully running before we send
   protocol commands. Schema creation needs a running Postgres.

2. **What if `reset` fails?**
   Proposed: fail the test with "reset failed" error. Don't continue
   with stale state — that hides bugs.

3. **Should non-reused services also support `seed`?**
   Proposed: yes. `seed` without `reuse` runs after every container
   create + healthcheck. Useful for schema creation even without reuse.

4. **Parallel reset?**
   Proposed: reset services in dependency order (like startup), not
   parallel. Simpler and avoids issues with reset callbacks that depend
   on other services.

5. **What about `faultbox generate` output?**
   Proposed: generated specs don't set `reuse`. The user adds it when
   they want performance. Auto-generating lifecycle hooks is too risky.

## References

- FB-015: Container reuse between tests
- FB-012: Seed data / init SQL for database services
- Current lifecycle: `internal/star/runtime.go` (`startServices`, `stopServices`)
- Container launch: `internal/container/launch.go`
