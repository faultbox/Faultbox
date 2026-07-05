# Chapter 25: Non-deterministic Operators - `choose`, `assume`, `halt`

**Duration:** 30 minutes
**Prerequisites:** [Chapter 14 (Invariants & Safety Properties)](14-invariants.md) completed, RFC-043 operators landed in v0.13.0

## Goals & Purpose

Chapter 14 taught you to write invariants that hold across every test. But you still wrote one test per scenario. When the scenario space is large - three retry strategies × four fault kinds × two timeout values - you'd write 24 separate tests and copy-paste the body across all of them.

The four operators in this chapter let you describe the *space* of scenarios as one test, and Faultbox produces 24 leaves at run time. Each leaf is its own deterministic execution with a unique `LeafID` and full bundle attribution. The plan tree shows exactly what was explored.

This chapter teaches you to:
- **`choose("name", [opts])`** - declare a finite N-way axis the plan tree fans out over
- **`assume(predicate)`** - filter the plan tree to only the leaves you care about
- **`halt(reason)`** - prune the current branch from inside the body
- **`nondet()`** - the non-deterministic boolean shorthand
- **The discovery run** - why your body runs once "for free" before fan-out

By the end you'll write one test that explores hundreds of scenarios without copy-paste.

## The motivating problem

You're testing a checkout service. Three things vary across your test matrix:

- **`retries`** - how many times the client retries on failure (0, 1, 3)
- **`fault`** - what the downstream returns (503, 504, timeout)
- **`backoff_ms`** - the delay between retries (10ms, 100ms)

In Chapter 14's style you'd write 3 × 3 × 2 = 18 tests. With RFC-043 operators you write one:

```python
def test_checkout_robustness():
    retries  = choose("retries", [0, 1, 3])
    fault    = choose("fault",   ["503", "504", "timeout"])
    backoff  = choose("backoff_ms", [10, 100])

    # Some combinations don't make sense - retries=0 with timeout
    # is uninteresting because there's nothing to retry.
    if retries == 0 and fault == "timeout":
        halt("nothing to retry - uninteresting branch")

    api.checkout(retries=retries, expected_fault=fault, backoff_ms=backoff)
```

That's 18 leaves minus the halted ones. Each leaf runs as its own test execution, gets its own bundle row, and shows up in the report with its axis values: `test_checkout_robustness [leaf 7 - retries=1, fault=2, backoff_ms=0]`.

## `choose("name", [opts])` - finite N-way axis

`choose()` is a Starlark builtin that returns one of the options. The first argument is the axis name (visible in the report); the second is the option list. The plan walker fans the test out across every option.

```python
def test_retries():
    n = choose("retries", [0, 1, 3])
    api.post("/order", retries=n)
```

Running this with `faultbox test spec.star` produces three leaves:
- `test_retries [leaf 0 - retries=0]` - body sees `n == 0`
- `test_retries [leaf 1 - retries=1]` - body sees `n == 1`
- `test_retries [leaf 2 - retries=2]` - body sees `n == 3`

> **Leaf-ID convention.** The display shows the *option index*, not the option *value*. For `choose("retries", [0, 1, 3])`, leaf 2 carries option index 2 - which decodes to the value 3. The full axis-value mapping is in the bundle's `plan.json`.

### Anonymous form

`choose([opts])` without a name still returns the first option, but doesn't fan out - there's no key for the plan tree to address the axis by:

```python
n = choose([10, 20, 30])    # always returns 10; no fan-out
n = choose("k", [10, 20, 30])  # 3 leaves
```

Use the named form whenever you want the axis exercised.

### Cross-product

Multiple `choose()` calls in one body produce the Cartesian product:

```python
def test_matrix():
    a = choose("a", [True, False])
    b = choose("b", [1, 2, 3])
    # 2 × 3 = 6 leaves
```

Mix freely with the other rc2 axes (`probability=` on faults, `interleavings=` on `parallel()` - see Chapter 26): cardinality multiplies.

## `assume(predicate)` - filter the plan tree

Sometimes the cross-product contains leaves you don't want to run. The classic case: a configuration constraint that only makes sense for some axis combinations.

```python
def test_checkout():
    retries  = choose("retries", [0, 1, 3, 5, 10])
    backoff  = choose("backoff_ms", [10, 100, 1000])
    api.checkout(retries=retries, backoff_ms=backoff)

# Only run the leaves where total wait time is sane.
test("checkout",
    body = test_checkout,
    assume = [lambda choices: choices["retries"] * choices["backoff_ms"] <= 3000],
)
```

The `assume=` kwarg on `test()` takes a list of predicates. Each predicate receives a `choices` dict mapping axis names to the leaf's selected option **value** (not the index - the dict carries what the body would observe from `choose()`, so a predicate that reads `choices["retries"]` against `choose("retries", [0, 1, 3])` sees one of `0`, `1`, `3` directly). Leaves where any predicate returns False are recorded as **halted** - they don't contribute to pass/fail.

> **Visibility:** Per-test `assume=` predicates evaluate at body entry. They see the leaf's axis assignment for any `choose()` call recorded during the discovery run (the first body execution; see below). Conditional choose() calls - axes that only appear in some branches - may not show up in `choices` for every leaf. Best practice: declare all axes unconditionally at the start of the body.

### Top-level `assume()`

You can also declare spec-wide constraints at the top level:

```python
retries_axis = choose("retries", [0, 1, 3])
fault_axis   = choose("fault", ["503", "504"])

# Reject the (retries=0, fault=504) combination spec-wide.
assume(lambda c: not (c.get("retries", 0) == 0 and c.get("fault", "") == "504"))

def test_checkout():
    api.checkout(retries=retries_axis, expected_fault=fault_axis)
```

Top-level `assume()` evaluates at spec load against the current axis snapshot. Use the per-test form (`test(..., assume=[...])`) when the constraint depends on per-leaf state.

### Predicate sandbox

`assume()` predicates are sandboxed at spec load: they can't call `fault()`, `service()`, `parallel()`, `halt()`, `eventually()`, `await_*`, or any other runtime-mutating builtin. The denylist mirrors RFC-041's monitor sandbox - predicates are pure functions of the `choices` argument.

Predicate Starlark errors (e.g. indexing a missing key) map to `Result="error"` (not `"fail"`), distinguishing spec-authoring bugs from actual SUT behavioral failures.

## `halt(reason="")` - prune from inside the body

`halt()` lets the body itself decide a leaf is uninteresting and prune it. Same effect as a false `assume()` predicate, but lives at the call site:

```python
def test_checkout():
    retries  = choose("retries", [0, 1, 3])
    fault    = choose("fault", ["503", "504", "timeout"])

    if retries == 0 and fault == "timeout":
        halt("nothing to retry - uninteresting branch")

    api.checkout(retries=retries, expected_fault=fault)
```

The halted leaf is recorded with `Result="halted"` - counted separately from pass/fail/inconclusive in the suite summary. Halt is not a verdict; CI gates that ignore halted runs see a clean exit.

### Where halt is rejected

- **Module top-level** - errors at spec load (`halt("…")` outside a function body).
- **Inside `setup=`** - runtime error before the body runs.
- **Inside a monitor `check=` / `update=` lambda** - produces an opaque "monitor violation: halt" message; use `assume()` to filter branches instead.

### halt() vs assume()

| | `halt()` | `assume()` |
|---|---|---|
| Where it lives | Body code | Spec preamble or `test(assume=[...])` |
| When it fires | At the call site during body execution | At body entry (per-test) or spec load (top-level) |
| Visibility | Can use any body-time state | Sees axis assignments only |
| Use for | "This combo doesn't make sense given runtime state I just computed" | "This combo is structurally uninteresting" |

## `nondet()` - non-deterministic boolean

`nondet()` is sugar for `choose([True, False])`. Like anonymous `choose()`, it doesn't fan out (no name), so it always returns `True`. Use the named form for fan-out:

```python
flaky = choose("flaky", [True, False])  # 2 leaves
if flaky:
    api.set_flaky_mode()
api.run()
```

> **Naming note:** the pre-RFC-043 `nondet(svc1, svc2, …)` form still works (it marks services as exempt from interleaving control during `parallel()`). The two arities are distinguished by argument count.

## The discovery run

Faultbox can't enumerate leaves without knowing what axes exist. So it runs your body **once** in *discovery* mode - a synthetic "leaf 0" that records every `choose()` call but skips `assume=` evaluation (the choices dict is empty at this point - predicates indexing a body-time key would spuriously error).

After discovery, the plan walker enumerates the real leaves and re-executes the body once per leaf with the explicit axis assignment pinned. Each re-execution sees the same `choose()` calls return their leaf-specific value.

What this means in practice:

- **The body runs N+1 times for N leaves**, not N. Discovery is leaf 0's prep.
- **`assume=` predicates fire only for the N real leaves**, not for discovery.
- **Side effects in the body fire N+1 times.** If your body emits a side effect - writing to a Go-side counter, mutating a fixture - be aware. Faultbox state (services, faults, event log) resets between leaves so the body sees a fresh environment each time.

## Putting it together - a real-world matrix

```python
api = service("api", binary="./api", interface("http", "http", 8080))
db  = service("db",  image="postgres:16-alpine")

# Module-level axes - visible to assume= predicates.
retries_axis = choose("retries", [0, 1, 3])
fault_axis   = choose("fault",   ["503", "504", "timeout"])

def checkout():
    if retries_axis == 0 and fault_axis == "timeout":
        halt("uninteresting: no retry budget for a timeout")
    api.http.post("/checkout", body='{"item":"widget"}')

test("checkout_matrix",
    body = checkout,
    assume = [
        lambda c: c.get("retries", 0) < 10,  # sanity cap
    ],
)
```

Running this produces 3 × 3 = 9 leaves; the halted branch (`retries=0, fault="timeout"`) is recorded as halted and skipped. The remaining 8 leaves run as 8 separate test executions, each with its own bundle row and trace.

The bundle's `plan.json` lists every axis and option set; the HTML report's tests table shows the per-leaf assignment in the row name. `faultbox plan spec.star` prints the tree without launching services - useful when you want to know "how many leaves will this produce?" before committing CI time.

## Exercises

1. **Halt the uninteresting half.** Add a third axis `protocol = choose("protocol", ["http", "https"])`. Use `halt()` to skip every `protocol="http"` leaf when `fault="timeout"`. Verify with `faultbox plan` that only the right leaves remain.

2. **Filter by predicate.** Rewrite Exercise 1 using a top-level `assume()` instead of `halt()`. Which approach reads better?

3. **Trigger a predicate error.** Write an `assume=` predicate that indexes `choices["nonexistent_key"]`. Run the test and verify `Result="error"` (not `"fail"`).

4. **Anonymous vs named.** Replace `choose("retries", [0, 1, 3])` with the anonymous form `choose([0, 1, 3])`. How many leaves does the test produce now? Why?

## What's next

[Chapter 26](26-plan-fanout.md) covers the RFC-042 plan-tree machinery these operators ride on: `faultbox plan`, probability fan-out, parallel-interleaving fan-out, and how to read the cross-product cost before CI runs it.
