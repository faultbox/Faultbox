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
def test_create_delivery():
    resp = courier.api.post(path="/deliveries", body='...')
    assert_eq(resp.status, 201)  # <-- hardcoded expectation
```

Under a `balance_down` fault, status 201 is wrong — 503 is the correct
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

1. **Scenario** = what users do (returns state, no judgment)
2. **Fault Assumption** = what goes wrong (reusable named fault set)
3. **Fault Scenario** = scenario + faults + expectation (the oracle)

### 1. Scenario Return Values

`scenario()` changes semantics: the registered function's return value is
captured and passed to the `expect` lambda in `fault_scenario()`.

```python
def create_delivery():
    resp = courier.api.post(path="/deliveries", body='{"from":"A","to":"B"}')
    return resp

scenario("create_delivery", create_delivery)
```

Multi-step scenarios return a dict with phase information:

```python
def delivery_lifecycle():
    create = courier.api.post(path="/deliveries", body='...')
    if create.status != 201:
        return {"phase": "create", "status": create.status}
    delivery_id = create.data["id"]
    assign = courier.api.post(path="/deliveries/" + delivery_id + "/assign",
                               body='{"driver":"d1"}')
    if assign.status != 200:
        return {"phase": "assign", "status": assign.status}
    complete = courier.api.post(path="/deliveries/" + delivery_id + "/complete")
    return {"phase": "complete", "status": complete.status}

scenario("delivery_lifecycle", delivery_lifecycle)
```

**Key:** Scenarios contain no `assert_eq`, `assert_eventually`, etc. They
are probes — they poke the system and report what happened.

### 2. `fault_assumption()` Builtin

Registers a named, reusable set of faults:

```python
fault_assumption("balance_slow",
    faults=[delay(bal, syscall="connect", duration="2s")],
    description="Balance API responds with 2s latency",
)

fault_assumption("balance_down",
    faults=[deny(bal, syscall="connect")],
    description="Balance API completely unreachable",
)

fault_assumption("cascade",
    faults=[
        delay(bal, syscall="connect", duration="2s"),
        deny(notif, syscall="connect"),
    ],
    description="Balance slow AND notifications down",
)
```

**Signature:**
```
fault_assumption(name: string, faults: list[FaultRule], description: string = "")
```

**Implementation:** Stores in `rt.faultAssumptions map[string]*FaultAssumptionDef`.

### 3. `fault_scenario()` Builtin

Composes a scenario with fault assumptions and an expectation:

```python
fault_scenario("happy_path",
    scenario="create_delivery",
    expect=lambda r: r.status == 201,
)

fault_scenario("delivery_balance_down",
    scenario="create_delivery",
    faults="balance_down",
    expect=lambda r: r.status == 503,
)

fault_scenario("lifecycle_db_slow",
    scenario="delivery_lifecycle",
    faults="db_slow",
    expect=lambda r: r["phase"] == "complete",
    timeout="10s",
)
```

**Signature:**
```
fault_scenario(
    name: string,
    scenario: string,              # name of registered scenario
    faults: string | list[string] = None,  # fault_assumption name(s)
    expect: callable = None,       # (result) -> bool
    timeout: string = "5s",
)
```

**Execution model:**
1. Apply fault rules from named assumption(s)
2. Run the scenario function
3. Capture return value
4. Call `expect(return_value)` — if returns falsy, test fails
5. Remove fault rules

If `expect` is None, default behavior: scenario completed without crash.

If `faults` is a list, all assumptions are applied simultaneously (combined).

### 4. `fault_matrix()` Builtin

Generates the cross-product of scenarios and fault assumptions:

```python
fault_matrix(
    scenarios=["create_delivery", "delivery_lifecycle", "rush_hour"],
    faults=["balance_slow", "balance_down", "db_slow"],
    default_expect=lambda r: True,  # default: didn't crash
    overrides={
        ("create_delivery", "balance_down"): lambda r: r.status == 503,
        ("delivery_lifecycle", "balance_down"): lambda r: r["phase"] == "create",
    },
)
```

**Signature:**
```
fault_matrix(
    scenarios: list[string],
    faults: list[string],
    default_expect: callable = None,
    overrides: dict = {},
)
```

**Behavior:** Generates one `fault_scenario` per cell. Name format:
`matrix_<scenario>_<fault>`. Each cell uses the override expect if present,
otherwise `default_expect`.

### 5. Matrix Report Output

Terminal output for fault matrix results:

```
Fault Matrix: 3 scenarios × 3 faults = 9 cells

                    │ balance_slow  │ balance_down  │ db_slow
────────────────────┼───────────────┼───────────────┼──────────────
create_delivery     │ PASS (8.2s)   │ PASS (0.3s)   │ PASS (3.1s)
delivery_lifecycle  │ PASS (9.1s)   │ PASS (0.1s)   │ PASS (4.2s)
rush_hour           │ PASS (10.3s)  │ FAIL          │ PASS (6.0s)

Result: 8/9 passed
Failures:
  rush_hour × balance_down: expected True, got crash (SIGSEGV)
```

JSON output (extends `--format json`):

```json
{
  "matrix": {
    "scenarios": ["create_delivery", "delivery_lifecycle", "rush_hour"],
    "faults": ["balance_slow", "balance_down", "db_slow"],
    "cells": [
      {
        "scenario": "create_delivery",
        "fault": "balance_slow",
        "passed": true,
        "result": {"status": 201},
        "duration_ms": 8200
      }
    ]
  }
}
```

## Backward Compatibility

### `test_*` functions still work

Existing `test_create_delivery()` functions with inline assertions are
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

## Implementation Plan

### Step 1: `fault_assumption()` builtin + registry

- Add `FaultAssumptionDef` type to `types.go`
- Add `rt.faultAssumptions` map to `Runtime`
- Register `fault_assumption` in predeclared builtins
- **Test:** Register + retrieve by name

### Step 2: `fault_scenario()` builtin + execution

- Add `FaultScenarioDef` type to `types.go`
- Add `rt.faultScenarios` map to `Runtime`
- Execution: apply faults → run scenario → capture return → check expect → remove faults
- Wire into `DiscoverTests()` as `test_<name>` (or separate runner)
- **Test:** Happy path, with fault, expect pass, expect fail

### Step 3: Scenario return value capture

- Modify `RunTest()` to capture Starlark return value
- Store in `TestResult.ScenarioResult starlark.Value`
- **Test:** Scenario returns dict, verify captured

### Step 4: `fault_matrix()` builtin

- Generate `fault_scenario` entries from cross-product
- Support `overrides` dict with `(scenario, fault)` tuple keys
- **Test:** 3×3 matrix, verify 9 tests generated

### Step 5: Matrix report format

- Terminal table output
- JSON output (extends existing `--format json`)
- **Test:** Verify formatting

## Open Questions

1. **Should `fault_scenario` be discoverable by `--test` filter?**
   Proposed: yes, `faultbox test my.star --test delivery_balance_down`

2. **Should `scenario()` still auto-register as `test_<name>`?**
   Proposed: yes, for backward compatibility. A bare scenario without
   fault_scenario acts as a happy-path test.

3. **Naming convention for matrix-generated tests?**
   Proposed: `matrix_<scenario>_<fault>` or `<scenario>__<fault>`

4. **Should `expect` receive the full TestResult or just the return value?**
   Proposed: just the return value (simpler). Monitors handle trace-level
   checks orthogonally.

5. **Should `fault_scenario` support multiple fault assumptions combined?**
   Proposed: yes, `faults=["balance_slow", "db_slow"]` applies both.

## References

- [Scenario & Fault Scenario Composition](https://www.notion.so/33f03cbd8e618128a2d0e5cfb29dc419) (Notion)
- Current `scenario()` implementation: `internal/star/builtins.go:790`
- Current `DiscoverTests()`: `internal/star/runtime.go:209`
- Current `ScenarioRegistration`: `internal/star/runtime.go:247`
