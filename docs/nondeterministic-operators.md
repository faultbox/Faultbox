# Non-deterministic Operators

> RFC-043's four operators — `choose()`, `nondet()`, `halt()`, `assume()` — give Faultbox specs the vocabulary P-lang and TLA+ have used for decades to describe a *space of behaviors*. v0.13.0-rc1 shipped the **language surface**; **rc2** wires the **plan-tree fan-out** for named `choose()` axes — each option becomes its own test execution with a stable `LeafID`. Anonymous `choose()` calls still return the first option (no name to address by). `nondet()` is sugar for `choose([True, False])` and inherits the same single-leaf behavior unless given a name. Assume-predicate AST denylist and full body-time choose() visibility to `assume=` predicates are still deferred — see the per-section notes below.

## Why these primitives

`fault_matrix`, `wait_all` interleavings, and `probability=` are each specialized expressions of one underlying concept: *the spec describes non-determinism, the plan engine enumerates the worlds*. RFC-043 names the concept directly with four small builtins:

| Operator | What it expresses |
|---|---|
| `choose([opts])` / `choose("name", [opts])` | Finite N-way choice. One world per option. |
| `nondet()` | Sugar for `choose([True, False])`. Non-deterministic boolean. |
| `halt()` | Prune the current branch. Body stops; outcome `"halted"`, not pass/fail. |
| `assume(predicate)` | Filter the plan tree. Branches where the predicate is false are pruned. |

Existing fan-out primitives stay — `fault_matrix` and friends keep working unchanged. The new operators sit alongside them; RFC-044 will eventually unify the engine machinery.

## `choose(options)` / `choose(name, options)`

```python
# Positional form — the choice is anonymous; report labels it by call site.
retries = choose([0, 1, 3])
api.create_order_with_retries(retries)

# Named form — name appears in plan.json and the report so assume()
# predicates and dashboards can refer to it.
retries = choose("retries", [0, 1, 3])

# Choice over service references (top-level — runs at spec load).
target = choose([primary_db, replica_db])
fault(target.main, deny=on(connect=True))
```

**Semantics:**
- **Named form** (`choose("retries", [0, 1, 3])`): the plan walker fans out one test execution per option. Each leaf observes its assigned option; the bundle's manifest carries a `leaf_id` per row.
- **Anonymous form** (`choose([0, 1, 3])`): returns the first option, no fan-out. There's no key to address the axis by — use the named form when you want plan-tree visibility.

`plan.json` records every call site and option set whether named or not.

**Constraints:**
- `options` must be a non-empty list literal. Empty lists are rejected at spec load.
- Runtime-computed elements (e.g. `choose([api.list_targets()])`) error because Starlark evaluates eagerly — the inner call fires at spec load before reaching `choose()`.

**Subsumes `param(name, choices=...)` from RFC-013** — RFC-044 withdraws `param`; `choose("name", [values])` is the replacement.

## `nondet()`

```python
def body():
    if nondet():
        api.path_a()
    else:
        api.path_b()
```

Sugar for `choose([True, False])`. The anonymous form returns `True` (the first option) without fan-out — `nondet()` has no name to address. Use a named choose to get the two-leaf fan-out: `flaky = choose("flaky", [True, False])`.

> **Naming note:** P-lang uses bare `$` as the non-deterministic-boolean operator. Starlark's lexer cannot parse `$`, and forking `go.starlark.net` for one character isn't worth the maintenance cost. `nondet()` is the canonical spelling.

> **Variadic form preserved:** `nondet(svc1, svc2, ...)` continues to mean *"exempt these services from interleaving control during `parallel()`"* (the pre-RFC-043 behavior). The two forms are distinguished by argument count. RFC-044 may revisit the unification.

## `halt()`

```python
def body():
    retries = choose([0, 1, 3])
    if retries == 0 and nondet():
        halt("invalid combo")      # this leaf is pruned
    api.run_workflow(retries=retries)
```

`halt()` terminates the current plan-tree branch. Body execution stops at the call site; the test outcome is `"halted"`, which is **distinct from pass / fail / inconclusive**:

- Suite-level `SuiteResult.Halted` counter
- Bundle manifest `summary.halted`
- HTML report row rendered with a grey pill (same palette as `fault_bypassed`)

**Constraints:**
- `halt()` is rejected outside a test body. Module-top-level calls error at spec load. Calls inside `setup=` are rejected at run time (before the body starts; the test is recorded as `"fail"`) with the same message: *"halt() may only be called inside a test body."*
- `halt()` inside a monitor `check=` / `update=` lambda is also unsupported — `inTest` is set during a test, so the call is reached, but the `*HaltError` propagates as a monitor violation (`Result="fail"`, reason `"monitor violation: halt"`). Treat this as an opaque error; use `assume()` to filter unwanted branches instead.
- An optional string reason is rendered in the trace/report.

## `assume(predicate)` and `test(assume=[...])`

```python
# Spec-wide constraint — evaluated at spec load.
assume(lambda choices: choices.get("retries", 0) < 5)

# Per-test constraint — evaluated at body entry.
test("ordered_check",
    body = body,
    assume = [
        lambda choices: choices["retries"] != 0 or choices["fault"] != "timeout",
    ],
)
```

The predicate receives a `choices` dict mapping each named `choose("name", opts)` call to its currently-selected option. Unnamed `choose()` calls are not visible to `assume` predicates.

> **rc1 visibility:** Per-test `assume=` predicates run **before the body** starts, so they only see choices recorded at spec load (top-level `choose(...)` calls). Choices made *inside the body* are recorded after the predicate has already executed and therefore are absent from `choices`. A predicate referencing a body-only key gets a `KeyError` → `Result="fail"`. rc2's per-leaf evaluation makes body-time choices visible.

**rc1 semantics:**
- Top-level `assume(False)` (or a lambda returning false) **fails spec load** with `"assume(...) violated at spec load."`
- Per-test `assume=` predicates **halt the test** (outcome `"halted"`) on the first failure.
- A predicate that raises a Starlark error (e.g. dict KeyError on a missing choice) **fails the test**.

**rc2** will defer evaluation to per-leaf and prune the plan tree instead of erroring at load.

**rc1 sandbox status:** predicates execute on a thread tagged `"assume:<name>"`, but **no builtin denylist is yet enforced** — a predicate today can call `fault()`, `service()`, `halt()`, or any runtime builtin. The AST-walk denylist (the same model as RFC-041's monitor `update`/`check` sandbox) lands in rc2 alongside §8.7. Treat predicates as pure functions of `choices` until then.

## Composition example

```python
db = service("db", image = "busybox", cmd = ["sh","-c","sleep 1"])

# Top-level choices — visible to assume= at body entry.
retries_axis = choose("retries", [0, 1, 3])
fault_axis   = choose("fault",   ["503", "504", "timeout"])

def scenario():
    if retries_axis == 0 and fault_axis == "timeout":
        halt("nothing to retry — uninteresting branch")
    api.run_workflow(retries=retries_axis, expected_fault=fault_axis)

test("matrix", body=scenario, assume=[
    # assume= predicates run at body entry against the top-level
    # choices dict. Body-time choices are NOT visible to predicates
    # in rc2 — declare the axes at module scope when you need them.
    lambda choices: choices.get("retries", 0) < 5,
])
```

At v0.13.0-rc2 this fans out to `3 × 3 = 9` leaves; the halted leaf (`retries=0, fault="timeout"`) is pruned (recorded as a `halted` row in the manifest). The `assume=` predicate trivially passes here because all `retries` values are < 5; if it returned False on a leaf, that leaf would be recorded as `halted` too. **rc2 limitation:** per-leaf re-evaluation of `assume=` that prunes false branches as the plan walks each axis is still deferred — see "Out of scope" below. Each surviving leaf is a separate test execution with its own bundle row, swim-lane trace, and verdict.

## Out of scope (deferred)

- **Body re-execution / plan-tree fan-out for `assume=`** — `assume=` predicates evaluate at body entry against the discovery-run choices only. Per-leaf evaluation pruning the plan tree on false predicates lands in a follow-up.
- **Weighted `choose([opts], weights=...)`** — probability-driven sampling. Not in v0.13.0.
- **Symbolic / parametric ranges** — `choose(range(0, MAX_INT))` needs SAT/SMT; indefinitely deferred.
- **Mazurkiewicz-trace independence** (RFC-009) and **DPOR** (RFC-010) — both operate on the unified plan tree this RFC's operators feed into.
- **Full assume-predicate AST sandbox** — rc2 alongside §8.7 (today predicates run in a tagged sandbox thread but the spec-load AST walk lands later).

## References

- RFC-043 — Non-deterministic Spec Operators (`docs/rfcs/0043-nondeterministic-operators.md`).
- RFC-042 — Exploration Plan & Coverage Engine. The plan-tree machinery these operators will fan out into.
- RFC-041 — Temporal Properties. `test()` declaration that gains the `assume=` kwarg here.
- RFC-040 — Determinism Levels. L0 plan determinism makes plan-tree enumeration coherent across runs.
- RFC-044 — Spec Language Simplification. Will withdraw `param()` and unify the fan-out machinery.
