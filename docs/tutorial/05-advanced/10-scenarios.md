# Chapter 10: Scenarios & Failure Generation

**Duration:** 20 minutes
**Prerequisites:** [Chapter 3 (Fault Injection)](../02-syscall-level/03-fault-injection.md) completed


## Goals & Purpose

[Chapter 6](../02-syscall-level/06-domain-model.md) introduced the domain-centric
model — separating scenarios, fault assumptions, and oracles into three
independent layers. This chapter builds on that with `faultbox generate`:
automatic failure discovery that outputs the domain-centric format.

**The question:** "Have I tested every way this system can break?"

This chapter teaches you to:
- **Register scenario probes** — describe what the system does
- **Auto-generate fault assumptions** — let Faultbox propose failure modes
- **Review and curate** — add overrides with expected behavior
- **Organize specs across files** — use `load()` for clean separation

After this chapter, your workflow becomes: write the happy path once,
generate all failures automatically, review, commit.

## The `scenario()` builtin

A scenario is a **probe** — a function that exercises the system and returns
an observable result. Register it with `scenario()`:

```python
# Linux (native): BIN = "bin"
# macOS (Lima): BIN = "bin/linux"
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
    """Write data through the API, read it back."""
    api.post(path="/data/mykey", body="myvalue")
    return api.get(path="/data/mykey")

scenario(order_flow)
```

Save this as `scenario-test.star`.

`scenario(order_flow)` does two things:
1. **Registers** the function for the failure generator and fault composition
2. **Runs it as a test** — equivalent to naming it `test_order_flow`

The return value is captured and available for use with `fault_scenario(expect=)`
and `fault_matrix(overrides=)`. See the "Fault Composition" section below.

Run it:
**Linux:**
```bash
faultbox test scenario-test.star
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test scenario-test.star"
```

```
--- PASS: test_order_flow (200ms, seed=0) ---

1 passed, 0 failed
```

## Generating failure scenarios

Now generate all possible failures for your scenarios:

**Linux:**
```bash
faultbox generate scenario-test.star
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox generate scenario-test.star"
```

```
wrote order_flow.faults.star
```

Look at the generated file:

```python
# order_flow.faults.star (generated)
load("scenario-test.star", "api", "db", "order_flow")

# --- Fault Assumptions ---

# network faults
db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
)

db_slow = fault_assumption("db_slow",
    target = api,
    connect = delay("5s"),
)

db_connection_reset = fault_assumption("db_connection_reset",
    target = api,
    read = deny("ECONNRESET"),
)

# disk faults
disk_io_error = fault_assumption("disk_io_error",
    target = db,
    write = deny("EIO"),
)

disk_full = fault_assumption("disk_full",
    target = db,
    write = deny("ENOSPC"),
)

fsync_failure = fault_assumption("fsync_failure",
    target = db,
    fsync = deny("EIO"),
)

# --- Fault Matrix ---

fault_matrix(
    scenarios = [order_flow],
    faults = [db_down, db_slow, db_connection_reset, disk_io_error, disk_full, fsync_failure],
)

# --- Network Partitions ---

def test_order_flow_db_partition():
    """order_flow with network partition between api and db."""
    partition(api, db, run=order_flow)
```

**What happened:** the generator created named `fault_assumption()` for each
fault mode and composed them into a `fault_matrix()`. Each assumption is
reusable — you can reference `db_down` in custom `fault_scenario()` calls.
No invented API calls, no guessed assertions — your exact happy path under
different failure conditions.

## Running generated tests

**Linux:**
```bash
faultbox test order_flow.faults.star
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox test order_flow.faults.star"
```

The generated tests run your scenario under each fault assumption.
The matrix report shows results at a glance:

```
Fault Matrix: 1 scenarios × 6 faults = 6 cells

                  │ db_down       │ db_slow       │ db_connection_reset │ disk_io_error │ disk_full     │ fsync_failure
──────────────────┼───────────────┼───────────────┼─────────────────────┼───────────────┼───────────────┼──────────────
order_flow        │ PASS (212ms)  │ PASS (5215ms) │ PASS (210ms)        │ PASS (210ms)  │ PASS (208ms)  │ PASS (210ms)

Result: 6/6 passed
```

How to read the results:

- **PASS** — the scenario completed without crashing under this fault.
  Since no `expect` was set, "pass" just means "didn't crash". Add
  `overrides=` to `fault_matrix()` to specify expected behavior per cell.
- **FAIL** — the scenario crashed or timed out under this fault.
  This is a **discovered failure mode** worth investigating.

To add expected behavior, edit the generated file and add overrides:

```python
fault_matrix(
    scenarios = [order_flow],
    faults = [db_down, db_slow, disk_io_error, disk_full, fsync_failure],
    overrides = {
        (order_flow, db_down): lambda r: assert_true(r.status >= 500),
        (order_flow, db_slow): lambda r: assert_true(r.duration_ms > 4000),
    },
    exclude = [
        (order_flow, fsync_failure),  # service doesn't use fsync
    ],
)
```

## The `load()` statement

Generated files use `load()` to import topology and scenario functions:

```python
load("scenario-test.star", "api", "db", "order_flow")
```

This means:
- Services are defined once (in your source file)
- Generated files share the same topology
- You can regenerate without affecting your source
- Multiple files can load from the same source

You can also use `load()` in hand-written files:

```python
# my-custom-failures.star
load("scenario-test.star", "api", "db", "order_flow")

db_down = fault_assumption("db_down",
    target = db,
    connect = deny("ECONNREFUSED"),
)

fault_scenario("order_custom_failure",
    scenario = order_flow,
    faults = db_down,
    expect = lambda r: assert_true(r.status >= 500, "should fail when DB is down"),
)
```

## Dry run — preview without generating

**Linux:**
```bash
faultbox generate scenario-test.star --dry-run
```

**macOS (Lima):**
```bash
make lima-run CMD="faultbox generate scenario-test.star --dry-run"
```

```
  order_flow × api: 4 mutations
  order_flow × db: 3 mutations
  Total: 7 mutations
```

## Multiple scenarios

Register as many scenarios as you want — each gets its own `.faults.star`:

```python
def order_flow():
    api.post(path="/data/mykey", body="myvalue")
    resp = api.get(path="/data/mykey")
    assert_eq(resp.body, "myvalue")

def health_check():
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)

scenario(order_flow)
scenario(health_check)
```

```bash
faultbox generate scenario-test.star
# → order_flow.faults.star
# → health_check.faults.star
```

## Fault composition — writing tests by hand

Auto-generation is great for discovery, but real tests need specific
expectations. That's what `fault_assumption()`, `fault_scenario()`,
and `fault_matrix()` are for.

### Named fault assumptions

Instead of repeating `fault(api, connect=deny("ECONNREFUSED"), run=...)`
everywhere, name it once:

```python
db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
)

disk_full = fault_assumption("disk_full",
    target = db,
    write = deny("ENOSPC"),
)
```

### Fault scenarios — one scenario, one fault, one oracle

```python
fault_scenario("order_db_down",
    scenario = order_flow,
    faults = db_down,
    expect = lambda r: assert_true(r.status >= 500, "should fail when DB is down"),
)
```

This registers `test_order_db_down`. The `expect` callback receives the
scenario's return value and validates it with assertions.

### Fault matrix — the cross-product

Instead of writing N×M tests by hand:

```python
fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [db_down, disk_full],
    default_expect = lambda r: assert_true(r != None),
    overrides = {
        (order_flow, db_down): lambda r: assert_true(r.status >= 500),
        (health_check, db_down): lambda r: assert_eq(r.status, 503),
    },
)
# Generates 4 tests: 2 scenarios × 2 faults
```

### Monitors on fault assumptions

Attach invariants to fault assumptions — they fire automatically in every
test that uses the assumption:

```python
def check_no_db_traffic(event):
    fail("traffic reached DB despite being down")

no_db_traffic = monitor(check_no_db_traffic, service="db", syscall="read")

db_down = fault_assumption("db_down",
    target = api,
    connect = deny("ECONNREFUSED"),
    monitors = [no_db_traffic],
)
```

Now every `fault_scenario()` and `fault_matrix()` cell that uses `db_down`
gets `no_db_traffic` automatically.

## The workflow

```
1. Write scenario() probes — describe how things work, return results
2. faultbox generate → creates <scenario>.faults.star with fault_matrix()
3. faultbox test *.faults.star → discover failures
4. Add overrides= with expected behavior per (scenario, fault) cell
5. Add monitors to fault_assumptions for invariants
6. Commit both source and .faults.star files
7. Regenerate when topology changes
```

## What you learned

- `scenario(fn)` registers a probe — runs as test + available for composition
- `faultbox generate` creates `fault_assumption()` + `fault_matrix()` per scenario
- `fault_assumption()` names and reuses fault configurations
- `fault_scenario()` composes probe + fault + expect oracle
- `fault_matrix()` generates the cross-product of scenarios × faults
- `load()` imports topology and functions across files
- Monitors travel with fault assumptions

## What's next

You've automated failure discovery and structured fault composition.
Chapter 11 introduces **event sources** — capturing structured stdout,
database WAL changes, and message queue events as first-class trace data.
