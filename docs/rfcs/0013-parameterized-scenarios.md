# RFC-013: Parameterized Scenarios & Value Generators

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.6
- **Created:** 2026-04-13
- **Branch:** `rfc/013-parameterized-scenarios`
- **Depends On:** RFC-001 (Scenarios as Probes)

## Summary

Allow scenarios to accept arguments and bind them to value generators
at registration time. The test engine expands each generator into
multiple scenario instances — one per value — and cross-multiplies
with `fault_matrix()` faults. This turns a single parameterized scenario
into a coverage matrix over both input space and failure space.

## Motivation

### Problem: Input-dependent behavior requires duplicated scenarios

Today, testing an API with different inputs requires either N separate
scenarios or one scenario that makes N sequential requests:

```python
# Option A: N duplicated scenarios.
def order_create_zero():
    return orders.post(path="/orders", body='{"sku":"widget","qty":0}')

def order_create_negative():
    return orders.post(path="/orders", body='{"sku":"widget","qty":-10}')

def order_create_large():
    return orders.post(path="/orders", body='{"sku":"widget","qty":1000}')

scenario(order_create_zero)
scenario(order_create_negative)
scenario(order_create_large)

# Option B: One scenario, N sequential requests — but they share state.
def order_create_all():
    results = {}
    for qty in [0, -10, 1000]:
        results[qty] = orders.post(path="/orders", body='{"sku":"widget","qty":' + str(qty) + '}')
    return results

scenario(order_create_all)
```

Option A is tedious. Option B conflates independent inputs into one test —
a failure at qty=0 aborts before qty=1000 runs, and fault injection applies
to all requests equally.

### What we want

```python
def order_create(qty):
    return orders.post(path="/orders", body='{"sku":"widget","qty":' + str(qty) + '}')

scenario(order_create, qty=values(-10, 0, 1, 1000, int_max, int_min))
```

This registers 6 independent scenario instances. Each runs in isolation,
each can be individually targeted by `fault_matrix()`, and each appears
as a separate test in the report.

### Scope

This RFC covers a **basic implementation** — explicit value sets and a small
library of edge-case generators. It does not cover:

- Shrinking (QuickCheck-style minimal counterexample search)
- Random generation with seed control (possible future extension)
- Stateful property-based testing
- Coverage-guided generation

## Design

### 1. Parameterized Scenario Registration

#### Change to `scenario()`

`scenario()` accepts keyword arguments that bind function parameters to
generators:

```
scenario(
    fn:       callable,                        # scenario function (may have parameters)
    **params: dict[string, Generator],         # parameter name → generator
) → None
```

When `**params` is empty, behavior is unchanged (zero-argument scenario).

When `**params` is present:
1. Each param name must match a parameter of `fn`.
2. Each param value must be a `Generator` (see below).
3. The engine computes the cross-product of all generators.
4. Each combination becomes a separate scenario instance.

#### Naming

Each instance is named `<fn_name>_<param1>=<value1>_<param2>=<value2>`:

```python
scenario(order_create, qty=values(0, -10, 1000))
# Registers:
#   test_order_create_qty=0
#   test_order_create_qty=-10
#   test_order_create_qty=1000
```

For multi-parameter scenarios:

```python
def transfer(amount, currency):
    return api.post(path="/transfer", body='{"amount":' + str(amount) + ',"currency":"' + currency + '"}')

scenario(transfer,
    amount   = values(0, 1, -1, 1000000),
    currency = values("USD", "EUR", ""),
)
# Registers 4 × 3 = 12 instances:
#   test_transfer_amount=0_currency=USD
#   test_transfer_amount=0_currency=EUR
#   test_transfer_amount=0_currency=
#   ... (12 total)
```

#### Interaction with `fault_matrix()`

Parameterized scenario instances are regular scenarios — `fault_matrix()`
treats each instance as a separate row:

```python
scenario(order_create, qty=values(0, -10, 1000))

fault_matrix(
    scenarios = [order_create],       # expands to all 3 instances
    faults = [inventory_down, disk_full],
    default_expect = lambda r: assert_true(r != None),
)
# Generates 3 × 2 = 6 tests
```

The matrix report shows each parameterized instance as a separate row:

```
                           │ inventory_down │ disk_full
───────────────────────────┼────────────────┼──────────────
order_create (qty=0)       │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=-10)     │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=1000)    │ PASS (0.1s)    │ PASS (0.2s)
```

---

### 2. Generator Builtins

Generators are first-class Starlark values that produce a finite set of
values for scenario parameterization.

#### Formal Type

```
Generator is a Starlark value with:
    .values() → list        # returns the finite list of values to iterate
    .name    → string       # human-readable name for test naming
```

#### `values(*args)` — Explicit Enumeration

The simplest generator. Returns exactly the values provided.

```
values(*args: any) → Generator
```

```python
values(0, 1, -10, 1000)            # [0, 1, -10, 1000]
values("widget", "gadget", "")      # ["widget", "gadget", ""]
values(True, False)                  # [True, False]
values(None, "", 0)                  # [None, "", 0] — falsy values
```

#### `int_edge(include_zero=True)` — Integer Boundary Values

Generates boundary values that commonly trigger off-by-one errors,
overflow, and sign-handling bugs.

```
int_edge(
    include_zero: bool = True,
) → Generator
```

Produces: `[0, 1, -1, int_max, int_min, int_max-1, int_min+1]`

Where `int_max = 2^63 - 1` and `int_min = -2^63` (Go int64 bounds,
matching the most common backend integer type).

If `include_zero=False`: omits `0`.

```python
scenario(order_create, qty=int_edge())
# 7 instances: qty=0, qty=1, qty=-1, qty=9223372036854775807, ...
```

#### `uint_edge()` — Unsigned Integer Boundary Values

```
uint_edge() → Generator
```

Produces: `[0, 1, uint_max, uint_max-1]`

Where `uint_max = 2^64 - 1`.

#### `string_edge()` — String Boundary Values

Generates strings that commonly trigger encoding, parsing, and validation
bugs.

```
string_edge() → Generator
```

Produces:

| Value | Why |
|-------|-----|
| `""` | Empty string — nil/null confusion |
| `" "` | Whitespace-only — trim bugs |
| `"a" * 1000` | Long string — buffer/truncation |
| `"<script>alert(1)</script>"` | XSS payload — escaping |
| `"'; DROP TABLE--"` | SQL injection — escaping |
| `"null"` | Literal "null" — JSON/nil confusion |
| `"0"` | Numeric string — type coercion |
| `"\n\t\r"` | Control characters — parsing |
| Unicode: `"é"`, `"日本"`, `"🎲"` | Multibyte — encoding |

#### `float_edge()` — Floating Point Boundary Values

```
float_edge() → Generator
```

Produces: `[0.0, -0.0, 1.0, -1.0, float_max, float_min, float_epsilon,
inf, -inf, nan]`

#### `bool_values()` — Boolean Exhaustive

```
bool_values() → Generator
```

Produces: `[True, False]`

#### `nullable(generator)` — Add None to Any Generator

Wraps another generator, prepending `None` to its value set.

```
nullable(generator: Generator) → Generator
```

```python
nullable(values(1, 2, 3))       # [None, 1, 2, 3]
nullable(int_edge())              # [None, 0, 1, -1, int_max, ...]
```

#### `combine(*generators)` — Union of Generators

Merges multiple generators into one (concatenates value sets, deduplicates).

```
combine(*generators: Generator) → Generator
```

```python
combine(values(42, 100), int_edge())   # [42, 100, 0, 1, -1, int_max, ...]
```

---

### 3. Generator Expansion

#### When Expansion Happens

Generators are expanded at **load time** (when `scenario()` is called),
not at test execution time. The expanded instances are registered in
`rt.scenarios` and are visible to `faultbox test --list`.

#### Cross-Product for Multiple Parameters

When a scenario has N parameters with generators of sizes S1, S2, ..., SN,
the total instances = S1 × S2 × ... × SN.

```python
scenario(transfer, amount=int_edge(), currency=values("USD", "EUR"))
# 7 × 2 = 14 instances
```

**Guard rail:** If the total exceeds 1000 instances, the runtime emits a
warning. If it exceeds 10000, it is a load-time error. These limits are
configurable via CLI flags.

#### Instance Identity

Each instance is a distinct scenario with a deterministic name. The name
encodes the parameter values, so:

- `faultbox test --test "order_create_qty=0"` runs one instance
- `faultbox test --test "order_create_qty=*"` runs all qty variants (glob)
- `faultbox test --list` shows all expanded instances

---

### 4. Full Example

```python
# --- Topology ---

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

# --- Parameterized Scenario ---

def order_create(qty):
    return orders.post(path="/orders", body='{"sku":"widget","qty":' + str(qty) + '}')

scenario(order_create, qty=values(-10, 0, 1, 1000, int_max, int_min))
# Registers 6 instances

# --- Fault Assumptions (from RFC-001) ---

inventory_down = fault_assumption("inventory_down",
    target = inventory,
    connect = deny("ECONNREFUSED"),
)

disk_full = fault_assumption("disk_full",
    target = inventory,
    write = deny("ENOSPC"),
)

# --- Matrix: 6 param values × 2 faults = 12 tests ---

fault_matrix(
    scenarios = [order_create],
    faults = [inventory_down, disk_full],
    default_expect = lambda r: assert_true(r != None),
    overrides = {
        (order_create, inventory_down): lambda r: assert_true(r.status >= 500),
    },
)
```

**Matrix report:**

```
Fault Matrix: 6 scenarios × 2 faults = 12 cells

                                │ inventory_down │ disk_full
────────────────────────────────┼────────────────┼──────────────
order_create (qty=-10)          │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=0)            │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=1)            │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=1000)         │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=9223372..)    │ PASS (0.1s)    │ PASS (0.1s)
order_create (qty=-922337..)    │ PASS (0.1s)    │ PASS (0.1s)

Result: 12/12 passed
```

## Backward Compatibility

- `scenario(fn)` with no kwargs: unchanged (zero-argument scenario)
- All new builtins (`values`, `int_edge`, etc.) are additive
- No existing behavior broken

## Inspiration

- **QuickCheck / QuickChick** — property-based testing with generators and
  shrinking. This RFC takes the generator concept but not shrinking (yet).
- **Hypothesis (Python)** — strategies for value generation. The `combine()`
  and `nullable()` combinators are inspired by Hypothesis composite strategies.
- **JUnit `@ParameterizedTest`** — parameterized test methods with value
  sources. The expansion model (each value = separate test) matches JUnit's
  approach.

## Future Extensions (Not In This RFC)

- **Shrinking:** When a parameterized instance fails, automatically search
  for a minimal failing input. Requires generators to define a shrink function.
- **Random generation with seed:** `int_random(min, max, count=100, seed=42)`
  — generate N random values, reproducible via seed.
- **Stateful sequences:** Generate sequences of API calls, not just single
  values. QuickCheck's state machine testing.
- **Coverage-guided generation:** Use code coverage feedback to steer
  generation toward untested paths.

## Open Questions

1. **Should `overrides` in `fault_matrix()` match all parameter values
   of a scenario or specific ones?**
   In the example above, `(order_create, inventory_down)` applies to all
   6 qty values. Should there be a way to override per-parameter-value?
   E.g., `(order_create(qty=0), inventory_down): lambda r: ...`

2. **Should generators be lazy or eager?**
   Proposed: eager (expand at load time). Lazy would allow infinite
   generators but complicates the execution model.

3. **Naming for large values — truncation?**
   `int_max` produces `qty=9223372036854775807` which is unwieldy.
   Proposed: truncate to `qty=922337..` in display, full value in JSON.

4. **Should `faultbox generate` produce parameterized scenarios?**
   Today the generator analyzes topology. It could also analyze API
   schemas (OpenAPI) to suggest parameter generators.

## Implementation Plan

### Step 1: Generator type + `values()` builtin

- Add `GeneratorDef` type to `types.go`
- Implement `values()` builtin — returns `GeneratorDef` wrapping a list
- **Test:** Create generator, access `.values()`

### Step 2: Parameterized `scenario()` expansion

- Extend `scenario()` to accept generator kwargs
- Cross-product expansion at load time
- Naming: `<fn>_<param>=<value>`
- Register expanded instances in `rt.scenarios`
- **Test:** Single param, multi param, `--list` output

### Step 3: Edge-case generators

- `int_edge()`, `uint_edge()`, `string_edge()`, `float_edge()`, `bool_values()`
- `nullable()`, `combine()`
- **Test:** Each generator produces expected values

### Step 4: Integration with `fault_matrix()`

- Parameterized instances work transparently as matrix rows
- Matrix report shows parameter values
- **Test:** Parameterized scenario × faults matrix

## References

- [RFC-001: Scenarios as Probes](0001-scenario-probes.md) — prerequisite
- [QuickCheck](https://hackage.haskell.org/package/QuickCheck) — property-based testing for Haskell
- [QuickChick](https://github.com/QuickChick/QuickChick) — property-based testing for Coq
- [Hypothesis](https://hypothesis.readthedocs.io/) — property-based testing for Python
