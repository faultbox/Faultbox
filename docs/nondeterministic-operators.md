# Non-deterministic Operators

> RFC-043's four operators — `choose()`, `nondet()`, `halt()`, `assume()` — give Faultbox specs the vocabulary P-lang and TLA+ have used for decades to describe a *space of behaviors*. v0.13.0-rc1 ships the **language surface** and the **runtime semantics for the single-leaf case**; rc2 alongside RFC-042 §8.8/§8.9 wires the **plan-tree fan-out** so each operator produces multiple test runs as designed.

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

**rc1 semantics:** returns the **first option** at runtime. `plan.json` records the call site and the option set. rc2's body-re-execution will run the body once per option and return the per-leaf selection.

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

Sugar for `choose([True, False])`. rc1 returns `True` (the first option) at runtime; rc2 fans out 2 leaves.

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
- `halt()` is rejected outside a test body. Calls at module top-level or inside `setup=` error at spec load with *"halt() may only be called inside a test body."*
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

**rc1 semantics:**
- Top-level `assume(False)` (or a lambda returning false) **fails spec load** with `"assume(...) violated at spec load."`
- Per-test `assume=` predicates **halt the test** (outcome `"halted"`) on the first failure.
- A predicate that raises a Starlark error (e.g. dict KeyError on a missing choice) **fails the test**.

**rc2** will defer evaluation to per-leaf and prune the plan tree instead of erroring at load.

**Predicates run in a sandbox starlark.Thread** (tagged `"assume:<name>"`). rc2 will add a full AST walk that rejects predicates referencing services, faults, or other runtime state — same model as RFC-041's monitor `update`/`check` sandbox.

## Composition example

```python
db = service("db", image = "busybox", cmd = ["sh","-c","sleep 1"])

def scenario():
    retries = choose("retries", [0, 1, 3])
    fault   = choose("fault",   ["503", "504", "timeout"])
    if retries == 0 and fault == "timeout":
        halt("nothing to retry — uninteresting branch")
    api.run_workflow(retries=retries, expected_fault=fault)

test("matrix", body=scenario, assume=[
    lambda choices: choices["retries"] < 5,
])
```

At rc1 this runs once (single leaf — first options): `retries=0, fault="503"`, no halt, no assume violation. At rc2 it fans out to `3 × 3 = 9` leaves minus halted (`retries=0, fault="timeout"`) and assume-pruned branches.

## Out of scope (deferred)

- **Body re-execution / plan-tree fan-out** — rc2 alongside RFC-042 §8.8.
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
