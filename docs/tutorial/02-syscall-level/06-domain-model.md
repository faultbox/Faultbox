# Chapter 6: From Tests to Domains

**Duration:** 20 minutes
**Prerequisites:** [Chapter 5 (Concurrency)](05-concurrency.md) completed

## The problem with test-centric thinking

In chapters 2-5 you wrote tests like this:

```python
def test_db_down():
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_eq(resp.status, 500)
    fault(db, connect=deny("ECONNREFUSED"), run=scenario)

def test_disk_full():
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_true(resp.status >= 500)
    fault(db, write=deny("ENOSPC"), run=scenario)

def test_slow_network():
    def scenario():
        resp = api.post(path="/data/key", body="value")
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 400)
    fault(db, write=delay("500ms"), run=scenario)
```

This works. But look at what happened:

1. **The scenario is duplicated three times** — `api.post(path="/data/key", body="value")`
   appears in every test, copied and pasted.
2. **The faults are inlined** — `connect=deny("ECONNREFUSED")` has no name. When you
   need "db down" in another test, you type it again.
3. **The assertions are embedded** — if you add a fourth fault, you copy-paste again.

With 5 scenarios and 4 fault modes, you have 20 hand-written test functions.
With 10 scenarios and 8 fault modes, you have 80. The approach doesn't scale.

## The domain-centric model

Faultbox v0.3 separates testing into three independent layers:

```
┌─────────────────────────────────────────────────┐
│  Layer 1: WHAT THE SYSTEM DOES (scenarios)      │
│                                                 │
│  def order_flow():                              │
│      return api.post(path="/orders", ...)       │
│                                                 │
│  def health_check():                            │
│      return api.get(path="/health")             │
├─────────────────────────────────────────────────┤
│  Layer 2: WHAT CAN GO WRONG (fault assumptions) │
│                                                 │
│  db_down = fault_assumption("db_down",          │
│      target=db, connect=deny("ECONNREFUSED"))   │
│                                                 │
│  disk_full = fault_assumption("disk_full",      │
│      target=db, write=deny("ENOSPC"))           │
├─────────────────────────────────────────────────┤
│  Layer 3: WHAT CORRECT MEANS (oracles)          │
│                                                 │
│  fault_matrix(                                  │
│      scenarios=[order_flow, health_check],      │
│      faults=[db_down, disk_full],               │
│      overrides={                                │
│          (order_flow, db_down): lambda r: ...   │
│      })                                         │
└─────────────────────────────────────────────────┘
```

Each layer is **defined once, reused everywhere**:

- A scenario describes a user action. It doesn't know about faults.
- A fault assumption describes a failure mode. It doesn't know about scenarios.
- The matrix combines them. You define expected behavior where it matters.

**5 scenarios + 4 faults = 9 definitions instead of 20 test functions.**

## Scenarios as probes

A scenario is a **probe** — it exercises the system and returns an observable
result. No assertions inside.

```python
BIN = "bin/linux"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", BIN + "/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def order_flow():
    """Place an order — return the response for external validation."""
    api.post(path="/data/mykey", body="myvalue")
    return api.get(path="/data/mykey")

scenario(order_flow)

def health_check():
    """Check API health — return the response."""
    return api.get(path="/health")

scenario(health_check)
```

Why no `assert_eq` inside? Because the same scenario runs under different
faults with different expected outcomes:

- Under no fault: `status == 200`
- Under `db_down`: `status >= 500`
- Under `slow_network`: `status == 200` but `duration_ms > 400`

The scenario doesn't judge — it just reports what happened.

## Named fault assumptions

Instead of typing `connect=deny("ECONNREFUSED")` everywhere, name it:

```python
db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
)

disk_full = fault_assumption("disk_full",
    target = db,
    write = deny("ENOSPC"),
)

slow_network = fault_assumption("slow_network",
    target = api,
    connect = delay("500ms"),
)
```

A fault assumption is a **reusable failure mode**. It carries:
- A name (for human readability and matrix reports)
- A target service
- The syscall-level faults to apply

You can also attach **monitors** — invariants that must hold whenever this
fault is active:

```python
def check_no_db_traffic(event):
    fail("traffic reached DB despite being down")

no_db_traffic = monitor(check_no_db_traffic, service="db", syscall="read")

db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
    monitors = [no_db_traffic],
)
```

Now every test that uses `db_down` automatically verifies that no traffic
reaches the DB. You write the invariant once.

## Fault scenarios — one scenario, one fault, one oracle

The simplest composition: pair one scenario with one fault assumption and
an `expect` oracle that validates the result:

```python
fault_scenario("order_db_down",
    scenario = order_flow,
    faults = db_down,
    expect = lambda r: assert_true(r.status >= 500, "should fail when DB is down"),
)
```

This registers `test_order_db_down`. When it runs:
1. Installs `db_down` fault rules (and its monitors)
2. Calls `order_flow()`, captures the return value
3. Passes the return value to `expect` — which asserts on it
4. Cleans up

**No fault (happy path oracle):**

```python
fault_scenario("order_happy",
    scenario = order_flow,
    expect = lambda r: (
        assert_eq(r.status, 200),
        assert_eq(r.body, "myvalue"),
    ),
)
```

Without `faults=`, the scenario runs under normal conditions — useful for
validating the happy path with explicit expectations.

**Smoke test (no oracle):**

```python
fault_scenario("order_disk_full_smoke",
    scenario = order_flow,
    faults = disk_full,
)
```

Without `expect=`, the test passes as long as the scenario completes
without crashing. Good for initial discovery — "does it survive this fault?"

**Multiple faults simultaneously:**

```python
cascade = fault_assumption("cascade",
    faults = [db_down, slow_network],
)

fault_scenario("order_cascade",
    scenario = order_flow,
    faults = cascade,
    expect = lambda r: assert_true(r.status >= 500),
)
```

`fault_scenario()` is the right tool when you have **one specific combination**
to test. When you have many scenarios × many faults, use `fault_matrix()`.

## The fault matrix — the cross-product

When you have multiple scenarios and multiple fault assumptions, the matrix
generates all combinations automatically:

```python
fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [db_down, disk_full, slow_network],
    default_expect = lambda r: assert_true(r != None, "must return a response"),
    overrides = {
        (order_flow, db_down): lambda r: assert_true(r.status >= 500),
        (order_flow, slow_network): lambda r: (
            assert_eq(r.status, 200),
            assert_true(r.duration_ms > 400),
        ),
        (health_check, db_down): lambda r: assert_true(r.status >= 500),
    },
)
```

This generates **6 tests** (2 scenarios × 3 faults):

```
Fault Matrix: 2 scenarios × 3 faults = 6 cells

                    │ db_down       │ disk_full     │ slow_network
────────────────────┼───────────────┼───────────────┼──────────────
order_flow          │ PASS (210ms)  │ PASS (208ms)  │ PASS (910ms)
health_check        │ PASS (206ms)  │ PASS (205ms)  │ PASS (705ms)

Result: 6/6 passed
```

**Cells without overrides** use `default_expect` — a baseline check
("must return something"). Cells with overrides use the specific oracle.

## When to use which approach

The domain-centric model doesn't replace the test-centric model — it builds
on top of it:

| Approach | When to use | Example |
|----------|------------|---------|
| `def test_*()` with inline `fault()` | Learning, debugging one specific case | Chapters 2-5 |
| `fault_scenario()` | One scenario + one fault + specific expected behavior | "When DB is down, order returns 503" |
| `fault_scenario()` (smoke) | Quick check: "does it survive this fault?" | No `expect=`, just no crash |
| `fault_matrix()` | Systematic coverage: many scenarios × many faults | 5 scenarios × 4 faults = 20 tests |
| `faultbox generate` | Discovery: let Faultbox propose failure modes | Auto-generates assumptions + matrix |

**Most users start with `fault_scenario()`** — it's the workhorse for
individual fault tests. **Graduate to `fault_matrix()`** when you have
multiple scenarios and faults that should be cross-tested.

## Composition — combining fault assumptions

Fault assumptions compose. Define simple ones and combine them:

```python
db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
)

slow_network = fault_assumption("slow_network",
    target = api,
    connect = delay("500ms"),
)

# Compound failure: DB down AND slow network simultaneously.
cascade = fault_assumption("cascade",
    faults = [db_down, slow_network],
    description = "DB down + slow network",
)
```

Use `cascade` in a matrix or scenario just like any single assumption.

## The full picture

Save this as `domain-test.star`:

```python
BIN = "bin/linux"

db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

api = service("api", BIN + "/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

# --- Layer 1: Scenarios (probes) ---

def order_flow():
    api.post(path="/data/mykey", body="myvalue")
    return api.get(path="/data/mykey")

scenario(order_flow)

def health_check():
    return api.get(path="/health")

scenario(health_check)

# --- Layer 2: Fault Assumptions (failure modes) ---

db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
)

disk_full = fault_assumption("disk_full",
    target = db,
    write = deny("ENOSPC"),
)

# --- Layer 3: Matrix (cross-product) ---

fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [db_down, disk_full],
)
```

Run it:

**Linux:**
```bash
faultbox test domain-test.star
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test domain-test.star"
```

You should see:

```
--- PASS: test_health_check (207ms, seed=0) ---
--- PASS: test_matrix_health_check_db_down (206ms, seed=0) ---
--- PASS: test_matrix_health_check_disk_full (208ms, seed=0) ---
--- PASS: test_matrix_order_flow_db_down (210ms, seed=0) ---
--- PASS: test_matrix_order_flow_disk_full (210ms, seed=0) ---
--- PASS: test_order_flow (208ms, seed=0) ---

Fault Matrix: 2 scenarios × 2 faults = 4 cells

                    │ db_down       │ disk_full
────────────────────┼───────────────┼──────────────
order_flow          │ PASS (210ms)  │ PASS (210ms)
health_check        │ PASS (206ms)  │ PASS (208ms)

Result: 4/4 passed

6 passed, 0 failed
```

4 matrix tests + 2 scenario tests = 6 tests from 2 scenarios and 2 fault
assumptions. Add a third fault and you get 6 matrix tests automatically.

## What you learned

- **Test-centric** works for small specs but duplicates scenario + fault + assertion
- **Domain-centric** separates WHAT (scenarios), WHAT BREAKS (assumptions), WHAT'S CORRECT (oracles)
- `scenario(fn)` registers a probe — returns observables, no assertions
- `fault_assumption()` names a reusable failure mode
- `fault_matrix()` generates the cross-product
- Monitors on assumptions enforce invariants across all tests
- Start with `def test_*()`, graduate to `fault_matrix()` when specs grow

## What's next

**From here forward, all tutorial examples use the domain-centric model.**
When you see a scenario, a fault assumption, or a matrix — that's the
standard approach for writing Faultbox specs.

Continue to:
- [Part 3: Protocol-Level Faults](../03-protocol-level/07-http-redis.md) — HTTP, database, broker faults
- [Part 4: Safety & Verification](../04-safety/14-invariants.md) — invariants, monitors, partitions
- [Part 5: Advanced Features](../05-advanced/09-containers.md) — containers, generation, event sources
