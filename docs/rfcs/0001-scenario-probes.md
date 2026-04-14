# RFC-001: Scenarios as Probes & Fault Scenario Composition

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.6
- **Created:** 2026-04-13
- **Branch:** `rfc/001-scenario-probes`

## Summary

Restructure the scenario/test model from "scenarios contain assertions" to
"scenarios are probes that return state; expectations are external per
fault combination." Introduce `fault_assumption()`, `fault_scenario()`, and
`fault_matrix()` builtins that separate what the system does, what goes
wrong, and what "correct" means.

## Motivation

### Problem 1: Scenarios are not reusable across faults

Today, assertions live inside test functions:

```python
def test_place_order():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)  # <-- hardcoded expectation
```

Under an `inventory_down` fault, status 200 is wrong — 503 is the correct
degradation behavior. The engineer must write a completely separate test
function with different assertions, duplicating the scenario logic.

### Problem 2: No fault matrix

Engineers want to answer "what happens to my 5 scenarios under 4 different
fault assumptions?" Today that requires 20 hand-written test functions.
With composition, it's 5 + 4 definitions.

### Problem 3: scenario() is sugar for test_*

Currently `scenario(fn)` just registers `fn` as `test_<name>` in globals
(`runtime.go:221`). There's no semantic difference between a scenario and
a test. This conflation prevents the system from knowing that a scenario
is a reusable action sequence vs. a test with embedded assertions.

## Design

### Core Principle: Three Layers

1. **Scenario** = what users do (returns observable state, contains no judgment)
2. **Fault Assumption** = what goes wrong (named, reusable, composable fault set)
3. **Fault Scenario / Fault Matrix** = scenario + faults + oracle (the test)

---

### 1. Scenario Return Values

#### Change

`scenario()` captures the registered function's return value and stores it
on the runtime as `ScenarioResult`. Today the return value is discarded
(`runtime.go:222` — the function is just called and registered as `test_<name>`).

#### Formal Signature

```
scenario(fn: callable) → None
```

- **fn** — a zero-argument Starlark callable. Its return value (any Starlark
  value) is captured by the runtime when the scenario is executed via
  `fault_scenario()` or `fault_matrix()`.
- Calling `scenario(fn)` still registers `fn` as `test_<fn.Name()>` for
  backward compatibility.
- If `fn` returns `None` (implicitly or explicitly), callers that access
  the result get `None`. This preserves backward compatibility with existing
  scenarios that have inline assertions and no explicit `return`.

#### Semantic Rule

> A scenario is a **probe**: it exercises the system under test and returns
> an observable. It SHOULD NOT contain `assert_*` calls. Assertions belong
> in the `expect` lambda of `fault_scenario()` or `fault_matrix()`.
>
> Existing scenarios with inline assertions remain valid — they are simply
> scenarios that also assert. The runtime does not enforce the rule;
> it is a convention.

#### Examples

```python
# Minimal probe — single observable.
def order_flow():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    return resp

scenario(order_flow)

# Multi-step probe — returns a dict of observables.
def order_lifecycle():
    place = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    if place.status != 200:
        return {"phase": "place", "resp": place}
    check_stock = orders.get(path="/inventory/widget")
    return {"phase": "check_stock", "resp": check_stock, "order": place}

scenario(order_lifecycle)
```

**Key:** Scenarios contain no `assert_eq`, `assert_eventually`, etc. They
are probes — they poke the system and report what happened.

---

### 2. `monitor()` as First-Class Object

#### Change

Today `monitor()` registers a callback and returns `None`. It can only be
used imperatively inside `test_*` functions. With the new model, monitors
need to be **reusable values** — attached to fault assumptions or fault
scenarios, not just called inline.

#### Formal Signature

```
monitor(
    callback: callable,              # (event: dict) → void; raise to fail
    service:  string = None,         # filter: service name (glob)
    syscall:  string = None,         # filter: syscall name (glob)
    path:     string = None,         # filter: file path (glob)
    decision: string = None,         # filter: decision (glob, e.g., "deny*")
    type:     string = None,         # filter: event type (e.g., "syscall", "proxy")
) → MonitorDef
```

**Returns:** A `MonitorDef` value — a first-class Starlark object that can
be stored in a variable, passed to `fault_assumption()` or `fault_scenario()`,
or used inline as before.

#### Callback Argument: Event Dict

The callback receives a single argument — a Starlark dict representing the
matched event. The dict contains:

| Key | Type | Description |
|-----|------|-------------|
| `"seq"` | int | Monotonic sequence number in the event log |
| `"type"` | string | Event type: `"syscall"`, `"proxy"`, `"lifecycle"`, etc. |
| `"service"` | string | Service that produced the event |
| `"syscall"` | string | Syscall name (e.g., `"write"`, `"connect"`) — present for syscall events |
| `"path"` | string | File path for file syscalls (e.g., `"/tmp/inventory.wal"`) |
| `"decision"` | string | Fault decision (e.g., `"allow"`, `"deny(EIO)"`, `"delay(500ms)"`) |
| `"label"` | string | Fault label if set (e.g., `"WAL write"`) |
| `"latency_ms"` | string | Latency in milliseconds (for delay faults) |
| + other `Fields` | string | Any additional event-specific fields |

All values are strings (following the current `ev.Fields` convention).

#### Behavior

- If the callback returns normally → event passes the monitor check.
- If the callback raises an error (via `fail()` or `assert_*`) → the test
  fails with "monitor violation: \<error message\>".
- Filters are applied **before** the callback — the callback only fires for
  matching events. Filter values support glob patterns.

#### Type: `MonitorDef`

A `MonitorDef` is a first-class Starlark value with:

| Attribute | Type | Description |
|-----------|------|-------------|
| `.callback` | callable | The monitor function |
| `.filters` | dict | The filter kwargs (service, syscall, path, decision, type) |

#### Backward Compatibility

Today `monitor()` returns `None` and registers immediately. With this change
it returns a `MonitorDef` and does **not** auto-register at call time.

To preserve backward compat for inline usage in `test_*` functions, the
runtime detects when `monitor()` is called inside a running test (not at
top level) and auto-registers it as before. At top level or when stored to
a variable, it's just a value.

#### Examples

```python
# First-class monitor — stored as a variable, reused across assumptions.
def check_no_wal_write(event):
    fail("unexpected WAL write while inventory is down: seq=" + str(event["seq"]))

no_wal_write = monitor(check_no_wal_write,
    service = "inventory",
    syscall = "openat",
    path = "/tmp/inventory.wal",
)

# Monitor with complex logic — hard to express in a lambda.
def check_retry_limit(event):
    # Track retries via a mutable container (Starlark closures are immutable).
    count = int(event.get("retry_count", "0"))
    if count > 3:
        fail("order service retried " + str(count) + " times, limit is 3")

retry_limit = monitor(check_retry_limit,
    service = "orders",
    syscall = "connect",
)

# Inline usage in test_* still works (backward compat).
def test_manual():
    monitor(lambda e: fail("bad") if e["decision"].startswith("deny") else None,
            service="inventory", syscall="write")
    orders.post(path="/orders", body='{"sku":"widget","qty":1}')
```

---

### 3. `fault_assumption()` Builtin

#### Purpose

Declares a named, reusable fault configuration. A `FaultAssumption` wraps
a target (service or interface) and a set of fault rules that can be applied
as a unit. Assumptions are composable — they can be combined by nesting.

#### Formal Signature

```
fault_assumption(
    name:        string,                         # unique identifier
    target:      ServiceDef | InterfaceRef,      # what to fault

    # --- Syscall-level faults (kwargs, extensible) ---
    # Any kwarg whose value is a FaultDef (deny/delay/allow) is treated
    # as a syscall-level fault.  The kwarg key is resolved as:
    #   1. A named operation on `target.ops` (e.g., "persist" → expand to
    #      the op's syscalls + path glob).  See op() in spec-language.md.
    #   2. A syscall family name from the fixed set (see below).
    #   3. A raw syscall name (e.g., "pwrite64").
    # This is the same resolution logic as fault().
    **syscall_faults: dict[string, FaultDef],

    # --- Protocol-level faults (keyword) ---
    rules:       list[ProxyFaultDef] = [],       # response/error/delay/drop/duplicate

    # --- Monitors (fault-level invariants) ---
    monitors:    list[MonitorDef] = [],          # invariants that must hold while this fault is active

    # --- Composition ---
    faults:      list[FaultAssumption] = [],     # merge child assumptions into this one

    # --- Metadata ---
    description: string = "",
) → FaultAssumption
```

#### Syscall Fault Keys: Resolution Order

When a kwarg key (e.g., `write`, `persist`, `pwrite64`) appears on
`fault_assumption()`, it is resolved exactly as `fault()` resolves it today:

| Step | Condition | Resolution |
|------|-----------|------------|
| 1 | Key matches `target.ops[key]` | Expand to the op's `syscalls` list, apply the op's `path` glob filter |
| 2 | Key is a syscall **family** name | Expand via `expandSyscallFamily()` |
| 3 | Key is a raw syscall name | Use as-is (no expansion) |

**Syscall families** (fixed set, defined in `runtime.go:937`):

| Family | Expands to |
|--------|------------|
| `write` | write, writev, pwrite64 |
| `read` | read, readv, pread64 |
| `open` | open, openat |
| `fsync` | fsync, fdatasync |
| `sendto` | sendto, sendmsg |
| `recvfrom` | recvfrom, recvmsg |

Families are **not user-extensible** — they are kernel syscall groupings.
Named operations (`op()` on a service) are the extensibility mechanism for
user-defined fault targets.

**Faultable syscalls** (all valid kwarg keys when not matching an op):

```
write, read, connect, openat, fsync, sendto, recvfrom,
writev, readv, close, pwrite64, pread64, fdatasync, sendmsg, recvmsg
```

Any key not matching an op or a known syscall is a **runtime error**.

#### FaultDef Values

Each kwarg value must be a `FaultDef`, produced by one of:

| Constructor | Semantics | Parameters |
|-------------|-----------|------------|
| `deny(errno, probability=, label=)` | Reject the syscall with the given errno | `errno`: string (e.g., `"ECONNREFUSED"`, `"EIO"`, `"ENOSPC"`). `probability`: string `"0%"`–`"100%"`, default `"100%"`. `label`: optional diagnostic string. |
| `delay(duration, probability=, label=)` | Pause the syscall for the given duration, then allow | `duration`: string (e.g., `"500ms"`, `"2s"`). Same probability/label. |
| `allow()` | Explicitly allow (no-op; useful to override a parent fault in composition) | — |

#### Protocol-Level Faults

When `target` is an `InterfaceRef` (e.g., `postgres.main`), the `rules`
keyword accepts a list of `ProxyFaultDef` values. These are protocol-aware
rules applied by the transparent proxy:

| Constructor | Protocol | Match fields | Action fields |
|-------------|----------|-------------|--------------|
| `error(query=, message=)` | postgres, mysql | `query` (glob) | `message` (error string) |
| `response(path=, status=, body=)` | http | `path` (glob), `method` | `status`, `body` |
| `delay(path=, delay=)` | any | protocol-specific match | `delay` (duration string) |
| `drop(topic=)` | kafka, nats | `topic` (glob) | — |
| `duplicate(topic=)` | kafka, nats | `topic` (glob) | — |

#### Combining Syscall-Level and Protocol-Level Faults

Syscall-level faults and protocol-level faults operate through **independent
mechanisms** — seccomp-notify and transparent proxy respectively. They can
coexist on the same service simultaneously without conflict.

Within a single `fault_assumption()` call, you can specify both syscall kwargs
and `rules=` as long as `target` is an `InterfaceRef` (which carries a
reference to the parent `ServiceDef` for syscall-level rules):

```python
# Both syscall and protocol faults on the same target.
pg_degraded = fault_assumption("pg_degraded",
    target = postgres.main,                              # InterfaceRef
    fsync = deny("EIO"),                                 # syscall-level: seccomp-notify on postgres
    rules = [error(query="INSERT*", message="disk full")],  # protocol-level: proxy on postgres.main
)
```

When `target` is a `ServiceDef` (not an `InterfaceRef`), only syscall kwargs
are valid — there's no interface to route `rules=` through. To combine
syscall faults on a service with protocol faults on its interface, use
composition:

```python
pg_write_fail = fault_assumption("pg_write_fail",
    target = postgres,
    write = deny("EIO"),
)

pg_insert_fail = fault_assumption("pg_insert_fail",
    target = postgres.main,
    rules = [error(query="INSERT*", message="disk full")],
)

# Composition: syscall + protocol on the same service.
pg_total_failure = fault_assumption("pg_total_failure",
    faults = [pg_write_fail, pg_insert_fail],
)
```

#### Composition Semantics

When `faults=[a, b, c]` is given, the resulting assumption merges all child
rules:

```
merged_rules = target_rules ∪ a.rules ∪ b.rules ∪ c.rules
```

Rules from different mechanisms (syscall vs protocol) never conflict — they
are applied through separate subsystems (`session.SetDynamicFaultRules()`
vs `proxyMgr.AddRule()`).

For rules within the same mechanism:
- **Syscall-level:** if the same `(target, syscall)` pair appears in multiple
  children, the **last one wins** (list order).
- **Protocol-level:** if the same `(target, interface, match_pattern)` appears
  in multiple children, the **last one wins** (list order).

If `target` is set on both the parent and a child, the parent's `target`
is used for the parent's own kwargs; each child retains its own target.
This allows multi-target assumptions.

#### Type: `FaultAssumption`

A `FaultAssumption` is a first-class Starlark value with:

| Attribute | Type | Description |
|-----------|------|-------------|
| `.name` | string | The unique name |
| `.rules` | list | Flattened list of `(target, FaultRule)` pairs |
| `.monitors` | list | `MonitorDef` values registered with this assumption |
| `.description` | string | Human-readable description |

#### Implementation

- New type `FaultAssumptionDef` in `types.go`
- Stored in `rt.faultAssumptions map[string]*FaultAssumptionDef`
- `fault()` extended: if first positional arg is a `FaultAssumption`,
  apply its rules instead of parsing kwargs

#### Examples

```python
# --- Monitors (first-class, reusable) ---

def check_no_wal_write(event):
    fail("unexpected WAL write while inventory is down: seq=" + str(event["seq"]))

no_wal_write = monitor(check_no_wal_write,
    service = "inventory",
    syscall = "openat",
    path = "/tmp/inventory.wal",
)

def check_no_inventory_traffic(event):
    fail("traffic reached inventory despite being unreachable")

no_inventory_traffic = monitor(check_no_inventory_traffic,
    service = "inventory",
    syscall = "read",
)

# --- Fault assumptions ---

# Syscall-level: single target, family key, with monitor.
inventory_down = fault_assumption("inventory_down",
    target = inventory,
    connect = deny("ECONNREFUSED"),
    monitors = [no_wal_write, no_inventory_traffic],
)

# Syscall-level: disk fault on inventory WAL.
disk_full = fault_assumption("disk_full",
    target = inventory,
    write = deny("ENOSPC"),
)

# Syscall-level: named operation (requires ops= on service definition).
# Given: inventory = service("inventory", ..., ops={"persist": op(syscalls=["write","fsync"], path="/tmp/*.wal")})
wal_corrupt = fault_assumption("wal_corrupt",
    target = inventory,
    persist = deny("EIO"),             # expands to write+fsync on /tmp/*.wal
)

# Syscall-level: latency.
slow_network = fault_assumption("slow_network",
    target = orders,
    connect = delay("200ms"),
    write = delay("100ms"),
)

# Protocol-level: transparent proxy rule.
pg_insert_fail = fault_assumption("pg_insert_fail",
    target = postgres.main,            # InterfaceRef
    rules = [error(query="INSERT*", message="disk full")],
)

# Composition: multiple targets, monitors from children are inherited.
cascade = fault_assumption("cascade",
    faults = [inventory_down, slow_network],
    description = "Inventory unreachable AND order service network slow",
    # cascade.monitors inherits no_wal_write + no_inventory_traffic from inventory_down
)
```

---

### 3. `fault_scenario()` Builtin

#### Purpose

Composes a scenario (probe) with a fault assumption and an expectation
function (oracle) to produce a runnable test.

#### Formal Signature

```
fault_scenario(
    name:     string,                                  # test name → test_<name>
    scenario: string | callable,                       # registered scenario name or fn
    faults:   FaultAssumption | list[FaultAssumption]  # fault(s) to apply
              | string | list[string] = None,          # or name(s) of registered assumptions
    expect:   callable = None,                         # (result) → void; asserts inside
    monitors: list[MonitorDef] = [],                   # scenario-level invariants
    timeout:  string = "5s",                           # max wall-clock time
) → None
```

#### Parameter Semantics

| Parameter | Resolution |
|-----------|------------|
| `scenario` | If string → lookup in `rt.scenarios[name]`. If callable → use directly. |
| `faults` | If `FaultAssumption` → use directly. If string → lookup in `rt.faultAssumptions[name]`. If list → merge all (union of rules; last-wins on conflict). If `None` → no faults (happy-path test). |
| `expect` | A callable `(result) → void`. It receives the scenario's return value. It should call `assert_*` builtins to validate. If it raises an error, the test fails. If `None` → the test passes as long as the scenario completes without crash/timeout. |
| `monitors` | List of `MonitorDef` values. These are scenario-level invariants — active only during this test, in addition to any monitors inherited from `faults`. |
| `timeout` | Parsed as Go duration. The scenario is cancelled if it exceeds this. |

#### Execution Model

```
1.  Resolve scenario function from name or callable
2.  Resolve and merge all fault assumptions
3.  Collect monitors: fault_assumption.monitors (all merged) + fault_scenario.monitors
4.  Register all collected monitors on the event log
5.  FOR EACH (target, rules) in merged assumptions:
        Install fault rules on target's running session
6.  result = call scenario function
7.  IF expect != None:
        call expect(result)           # expect calls assert_* to validate
8.  FOR EACH target:
        Remove fault rules
9.  Unregister all monitors
10. Test passes if no assertion error was raised in steps 6–7
    AND no monitor violation was raised during step 6
```

**Monitor precedence:** Fault-assumption monitors fire first (registered
in assumption merge order), then fault-scenario monitors. All monitors
are active for the entire duration of the scenario (step 6). A monitor
violation during the scenario immediately fails the test — `expect` is
not called.

**Error semantics:**
- Monitor violation during scenario → test fails with "monitor violation: \<message\>"
- Scenario crash (panic, SIGSEGV) → test fails with crash report
- Scenario timeout → test fails with timeout error
- `expect` raises assertion error → test fails with assertion message
- `expect` returns any value → ignored (it validates via side-effects)

#### Test Registration

`fault_scenario()` is called at **load time** (top-level Starlark execution).
It registers a test entry in `rt.tests` with the name `test_<name>`.
The test is discoverable by `faultbox test --list` and filterable by
`faultbox test --test <name>`.

#### Examples

```python
# Happy path (no faults, just the oracle).
fault_scenario("order_happy",
    scenario = order_flow,
    expect = lambda r: assert_eq(r.status, 200),
)

# Single fault assumption.
fault_scenario("order_inventory_down",
    scenario = order_flow,
    faults = inventory_down,
    expect = lambda r: (
        assert_eq(r.status, 503),
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal"),
    ),
)

# Combined faults.
fault_scenario("order_cascade",
    scenario = order_flow,
    faults = [inventory_down, slow_network],
    expect = lambda r: assert_true(r.status >= 500),
)

# Smoke test (no oracle — just "must not crash").
fault_scenario("order_disk_full_smoke",
    scenario = order_lifecycle,
    faults = disk_full,
)

# With timeout for slow-network scenarios.
fault_scenario("order_slow_completes",
    scenario = order_flow,
    faults = slow_network,
    expect = lambda r: (
        assert_eq(r.status, 200),
        assert_true(r.duration_ms > 100, "expected delay > 100ms"),
    ),
    timeout = "10s",
)

# Scenario-level monitor: check retry behavior specific to this test.
# inventory_down already carries no_wal_write + no_inventory_traffic monitors
# from its fault_assumption definition — those fire automatically.
# This adds an extra scenario-specific invariant on top.
def check_retry_limit(event):
    if int(event.get("retry_count", "0")) > 3:
        fail("order service retried too many times: " + event["retry_count"])

retry_monitor = monitor(check_retry_limit, service="orders", syscall="connect")

fault_scenario("order_inventory_down_retries",
    scenario = order_flow,
    faults = inventory_down,         # inherits no_wal_write + no_inventory_traffic
    monitors = [retry_monitor],      # adds scenario-specific retry check
    expect = lambda r: assert_eq(r.status, 503),
)
```

---

### 4. `fault_matrix()` Builtin

#### Purpose

Generates the cross-product of scenarios × fault assumptions, producing
one `fault_scenario` per cell. Reduces N×M hand-written tests to a single
declaration.

#### Formal Signature

```
fault_matrix(
    scenarios:      list[string | callable],                       # probes
    faults:         list[FaultAssumption | string],                # assumptions
    default_expect: callable = None,                               # (result) → void
    overrides:      dict[(callable, FaultAssumption), callable]    # cell-specific oracles
                    = {},
    monitors:       list[MonitorDef] = [],                         # matrix-wide invariants
    exclude:        list[(callable, FaultAssumption)] = [],        # cells to skip
) → None
```

#### Parameter Semantics

| Parameter | Description |
|-----------|-------------|
| `scenarios` | List of scenario functions or registered names. Each must be a valid `scenario()` registration. |
| `faults` | List of `FaultAssumption` values or registered names. |
| `default_expect` | Oracle applied to every cell that has no override and is not excluded. If `None`, cells without overrides are smoke tests (must not crash). |
| `overrides` | Dict mapping `(scenario, fault)` tuples to cell-specific `expect` callables. The tuple keys reference the same objects/names used in `scenarios` and `faults`. |
| `monitors` | List of `MonitorDef` values applied to every cell in the matrix. These are in addition to monitors from each cell's `fault_assumption`. |
| `exclude` | List of `(scenario, fault)` tuples to skip entirely (no test generated). |

#### Expansion Semantics

```
FOR scenario IN scenarios:
    FOR fault IN faults:
        IF (scenario, fault) IN exclude:
            SKIP
        expect = overrides.get((scenario, fault), default_expect)
        EMIT fault_scenario(
            name     = "matrix_" + scenario.name + "_" + fault.name,
            scenario = scenario,
            faults   = fault,
            expect   = expect,
            monitors = matrix.monitors,   # fault.monitors are inherited automatically
        )
```

- Generated test count: `|scenarios| × |faults| − |exclude|`
- Name format: `test_matrix_<scenario_name>_<fault_name>`
- All generated tests are discoverable by `--list` and filterable by `--test`

#### Override Precedence

```
cell-specific override  >  default_expect  >  None (smoke test)
```

#### Examples

```python
fault_matrix(
    scenarios = [order_flow, order_lifecycle, health_check],
    faults = [inventory_down, disk_full, slow_network],
    default_expect = lambda r: assert_true(r != None, "must return a response"),
    overrides = {
        (order_flow, inventory_down): lambda r: (
            assert_eq(r.status, 503),
            assert_true("unreachable" in r.body),
            assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal"),
        ),
        (order_flow, slow_network): lambda r: (
            assert_eq(r.status, 200),
            assert_true(r.duration_ms > 100, "expected delay from slow network"),
        ),
        (health_check, inventory_down): lambda r: (
            assert_eq(r.status, 503),
        ),
    },
    exclude = [
        (health_check, disk_full),  # health check doesn't touch disk
    ],
)
# Generates 8 tests: 3×3 - 1 excluded
```

---

### 5. Matrix Report Output

When the test run includes `fault_matrix()`-generated tests, the runner
outputs a matrix summary after the standard test results.

**Terminal output:**

```
Fault Matrix: 3 scenarios × 3 faults = 8 cells (1 excluded)

                    │ inventory_down │ disk_full     │ slow_network
────────────────────┼────────────────┼───────────────┼──────────────
order_flow          │ PASS (0.3s)    │ PASS (0.1s)   │ PASS (0.3s)
order_lifecycle     │ PASS (0.1s)    │ PASS (0.2s)   │ PASS (1.2s)
health_check        │ PASS (0.1s)    │ — (excluded)  │ PASS (0.4s)

Result: 8/8 passed
```

**JSON output** (extends `--format json`):

```json
{
  "matrix": {
    "scenarios": ["order_flow", "order_lifecycle", "health_check"],
    "faults": ["inventory_down", "disk_full", "slow_network"],
    "cells": [
      {
        "scenario": "order_flow",
        "fault": "inventory_down",
        "passed": true,
        "duration_ms": 300,
        "result_summary": {"status": 503}
      },
      {
        "scenario": "order_flow",
        "fault": "disk_full",
        "passed": true,
        "duration_ms": 100,
        "result_summary": {"status": 500}
      }
    ],
    "excluded": [["health_check", "disk_full"]],
    "total": 8,
    "passed": 8,
    "failed": 0
  }
}
```

---

### Full Example: Before and After

**Before (current v0.2 style) — 6 test functions with duplicated logic:**

```python
inventory = service("inventory",
    "/tmp/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders",
    "/tmp/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

def test_happy_path():
    resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
    assert_eq(resp.status, 200)
    assert_true("confirmed" in resp.body)

def test_inventory_down():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 503)
        assert_true("unreachable" in resp.body)
        assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)

def test_disk_full():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status >= 500, "expected 5xx on ENOSPC")
    fault(inventory, write=deny("ENOSPC"), run=scenario)

def test_slow_network():
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 100, "expected delay > 100ms")
    fault(orders, connect=delay("200ms"), run=scenario)

def test_health_check():
    resp = orders.get(path="/health")
    assert_eq(resp.status, 200)

def test_health_check_inventory_down():
    def scenario():
        resp = orders.get(path="/health")
        assert_eq(resp.status, 503)
    fault(orders, connect=deny("ECONNREFUSED"), run=scenario)
```

**After (RFC-001 style) — same coverage, no duplication:**

```python
# --- Topology (unchanged) ---

inventory = service("inventory",
    "/tmp/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders",
    "/tmp/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

# --- Probes (scenarios as pure observables) ---

def order_flow():
    return orders.post(path="/orders", body='{"sku":"widget","qty":1}')

scenario(order_flow)

def health_check():
    return orders.get(path="/health")

scenario(health_check)

# --- Fault Assumptions (named, reusable) ---

inventory_down = fault_assumption("inventory_down",
    target = orders,
    connect = deny("ECONNREFUSED"),
)

disk_full = fault_assumption("disk_full",
    target = inventory,
    write = deny("ENOSPC"),
)

slow_network = fault_assumption("slow_network",
    target = orders,
    connect = delay("200ms"),
)

# --- Fault Matrix (cross-product) ---

fault_matrix(
    scenarios = [order_flow, health_check],
    faults = [inventory_down, disk_full, slow_network],
    default_expect = lambda r: assert_true(r != None, "must return a response"),
    overrides = {
        (order_flow, inventory_down): lambda r: (
            assert_eq(r.status, 503),
            assert_true("unreachable" in r.body),
            assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal"),
        ),
        (order_flow, disk_full): lambda r: (
            assert_true(r.status >= 500, "expected 5xx on ENOSPC"),
        ),
        (order_flow, slow_network): lambda r: (
            assert_eq(r.status, 200),
            assert_true(r.duration_ms > 100, "expected delay > 100ms"),
        ),
        (health_check, inventory_down): lambda r: (
            assert_eq(r.status, 503),
        ),
    },
)
```

---

### Interaction with `faultbox generate`

`faultbox generate` continues to work but now produces `fault_assumption()`
and `fault_matrix()` calls instead of flat `test_gen_*` functions:

**Current output (v0.2):**

```python
# order_flow.faults.star (auto-generated)
load("faultbox.star", "orders", "inventory", "order_flow")

def test_gen_order_flow_inventory_down():
    """order_flow with inventory connection refused."""
    fault(orders, connect=deny("ECONNREFUSED", label="inventory down"), run=order_flow)

def test_gen_order_flow_inventory_slow():
    """order_flow with inventory connection delayed 500ms."""
    fault(orders, connect=delay("500ms", label="inventory slow"), run=order_flow)

def test_gen_order_flow_disk_eio():
    """order_flow with inventory write EIO."""
    fault(inventory, write=deny("EIO", label="disk eio"), run=order_flow)

def test_gen_order_flow_disk_enospc():
    """order_flow with inventory write ENOSPC."""
    fault(inventory, write=deny("ENOSPC", label="disk full"), run=order_flow)
```

**New output (RFC-001):**

```python
# order_flow.faults.star (auto-generated)
load("faultbox.star", "orders", "inventory", "order_flow")

inventory_down = fault_assumption("inventory_down",
    target = orders,
    connect = deny("ECONNREFUSED"),
)

inventory_slow = fault_assumption("inventory_slow",
    target = orders,
    connect = delay("500ms"),
)

disk_eio = fault_assumption("disk_eio",
    target = inventory,
    write = deny("EIO"),
)

disk_enospc = fault_assumption("disk_enospc",
    target = inventory,
    write = deny("ENOSPC"),
)

fault_matrix(
    scenarios = [order_flow],
    faults = [inventory_down, inventory_slow, disk_eio, disk_enospc],
)
```

## Backward Compatibility

### `test_*` functions still work

Existing `test_place_order()` functions with inline assertions are
unchanged. They run as before. This is additive.

### `scenario()` return values are new

Currently `scenario()` ignores the return value (the function is just
registered and run as a test). The change: we now capture the return value.
Old scenarios that return `None` implicitly still work — `expect` receives
`None` and can handle it.

### No existing behavior broken

- `faultbox test my.star` runs all `test_*` and `scenario()` functions as before
- New `fault_scenario()` / `fault_matrix()` are additive builtins
- Old specs don't use them and are unaffected
- `fault()` gains the ability to accept a `FaultAssumption` as first arg — additive

## Implementation Plan

### Step 1: Scenario return value capture

- Modify `scenario()` to capture Starlark return value in `ScenarioResult`
- Add `ScenarioResult` type to Starlark runtime
- Update `DiscoverTests` to propagate return values
- **Test:** Scenario returns dict, verify captured; scenario returns `None`, verify backward compat

### Step 2: `fault_assumption()` builtin + registry

- Add `FaultAssumptionDef` type to `types.go`
- Add `rt.faultAssumptions` map to `Runtime`
- Register `fault_assumption` in predeclared builtins
- Implement kwarg resolution: named op → family → raw syscall
- Implement composition via `faults=` with merge semantics
- Extend `fault()` to accept `FaultAssumption` as first positional arg
- **Test:** Register + retrieve by name, composition, named ops, protocol-level

### Step 3: `fault_scenario()` builtin + execution

- Add `FaultScenarioDef` type to `types.go`
- Add `rt.faultScenarios` map to `Runtime`
- Execution: apply faults → run scenario → capture return → check expect → remove faults
- Wire into `DiscoverTests()` as `test_<name>` (or separate runner)
- **Test:** Happy path, with fault, expect pass, expect fail, smoke test (no expect)

### Step 4: `fault_matrix()` builtin

- Generate `fault_scenario` entries from cross-product
- Support `overrides` dict with `(scenario, fault)` tuple keys
- Support `exclude` list
- **Test:** 3×3 matrix, verify generated test count; overrides; excludes

### Step 5: Matrix report format + generator update

- Terminal table output when matrix tests are present
- JSON output (extends existing `--format json`)
- Update `internal/generate/codegen.go` to emit `fault_assumption()` + `fault_matrix()`
  instead of flat `test_gen_*` functions
- **Test:** Verify formatting, generator output comparison

## Open Questions

1. **Should `expect` receive the event log as a second argument?**
   `expect = lambda result, events: ...` would allow temporal assertions
   without global `assert_eventually()`. Tradeoff: simpler signature vs.
   co-located trace checks.
   Proposed: No. `assert_eventually()` / `assert_never()` already query
   the global event log. Adding `events` to `expect` creates two ways to
   do the same thing.

2. **Should `fault_assumption()` support top-level `probability` modifier?**
   E.g., `fault_assumption("flaky", target=inventory, connect=deny("ECONNREFUSED"), probability="20%")`.
   Today probability is per-FaultDef (`deny(..., probability="20%")`).
   A top-level modifier would override all contained FaultDefs.
   Proposed: Defer. Per-FaultDef probability is sufficient.

3. **Naming: `fault_assumption` vs `fault_preset` vs `fault_profile`?**
   "Assumption" aligns with formal verification terminology (a precondition
   on the environment). "Preset" is more intuitive for new users.
   Proposed: `fault_assumption` — the formal alignment matters for the
   Phase 3 trace-equivalence work (RFC-009+).

4. **Should `scenario()` still auto-register as `test_<name>`?**
   Proposed: Yes, for backward compatibility. A bare `scenario()` without
   a corresponding `fault_scenario` acts as a happy-path test.

5. **Conflict resolution in composition: last-wins or error?**
   When `faults=[a, b]` and both target the same `(service, syscall)`,
   proposed behavior is last-wins (b overrides a). Alternative: raise
   an error to force explicit resolution.

## References

- [Scenario & Fault Scenario Composition](https://www.notion.so/33f03cbd8e618128a2d0e5cfb29dc419) (Notion)
- Current `scenario()` implementation: `internal/star/builtins.go:790`
- Current `DiscoverTests()`: `internal/star/runtime.go:209`
- Current `ScenarioRegistration`: `internal/star/runtime.go:247`
- Current `expandSyscallFamily()`: `internal/star/runtime.go:937`
- Current `builtinFault()`: `internal/star/builtins.go:390`
- Current `faultableSyscalls`: `internal/star/runtime.go:957`
