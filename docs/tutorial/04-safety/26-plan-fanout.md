# Chapter 26: Plan-Tree Fan-Out - `faultbox plan`, probability, interleavings

**Duration:** 30 minutes
**Prerequisites:** [Chapter 25 (Non-deterministic Operators)](25-choose-and-assume.md) completed, [Chapter 5 (Exploring Concurrency)](../02-syscall-level/05-concurrency.md) reviewed

## Goals & Purpose

Chapter 25's `choose()` taught you to declare a finite axis the plan tree fans out over. This chapter shows the rest of the rc2 fan-out machinery: **probability fan-out** on faults (every fired/not-fired combination), **interleaving fan-out** on `parallel()` (every branch ordering), and the `faultbox plan` command that prints the cross-product cost *before* you commit CI minutes to it.

The three axis kinds compose. A spec with two `choose()` axes, one probabilistic fault with `max_fires=3`, and one `parallel(interleavings="all")` with 3 branches produces `2 × 2 × 2³ × 3! = 192` leaves. Reading that number out of `faultbox plan` is cheaper than discovering it from a 20-minute CI run.

This chapter teaches you to:
- **Probability fan-out** - turn stochastic fault firing into exhaustive coverage
- **Interleaving fan-out** - explore every branch ordering of `parallel()`
- **`faultbox plan`** - preview the plan tree without launching services
- **Reading the report** - find a specific leaf's trace in a bundle

## Probability fan-out - `max_fires` + `mode="exhaustive"`

Pre-rc2 Faultbox treated `probability=p` stochastically: at each trigger point the seeded RNG decided whether the rule fired this time. A single test run produced one realization. Reproducing a rare bug required hunting for the right seed.

rc2 adds `max_fires=N` and `mode="exhaustive"` to `delay()` and `deny()`. Together they turn probability into a fan-out axis: every fired/not-fired combination across the first N occurrences becomes its own plan-tree leaf with a deterministic per-occurrence vector.

```python
fault(db,
    write = deny("EIO",
        probability = 0.3,
        max_fires   = 2,
        mode        = "exhaustive",
        label       = "wal",
    ),
    run = lambda: api.checkout(),
)
```

This produces `2² = 4` leaves:

| Leaf | Occurrence 1 | Occurrence 2 |
|------|--------------|--------------|
| 0 | not fired | not fired |
| 1 | fired | not fired |
| 2 | not fired | fired |
| 3 | fired | fired |

Each leaf is a deterministic execution. The bundle records the per-occurrence vector under `leaf_probability_outcomes["wal"]`; the report's tests table shows `wal[10]` (bit string) on each row.

### Mode rules

| Combination | Behavior |
|-------------|----------|
| `probability=1.0` | Always fires. No fan-out. `max_fires=` rejected. |
| `probability<1, max_fires=` unset | Stochastic - legacy RNG path. |
| `probability<1, max_fires=N` | Exhaustive - `2^N` leaves with deterministic vector. Default mode. |
| `probability<1, max_fires=N, mode="stochastic"` | Rejected at spec load - `max_fires=` is incompatible with stochastic. |
| `probability<1, mode="exhaustive"` (no `max_fires`) | Rejected at spec load - would silently degrade to stochastic. |

### Bare `probability=p` is unaffected

This is the migration story. Existing specs that use `probability=0.3` *without* `max_fires=` keep the stochastic RNG path verbatim - no behavior change, no surprise extra leaves in CI. Opting into exhaustive fan-out is an explicit `max_fires=N` add.

> **Scope note:** rc2 probability fan-out is syscall-level (`delay()` / `deny()`). Protocol-level (`response()` / `error()` / `drop()`) still uses the stochastic path; the `max_fires=` surface threads through `internal/proxy` in a follow-up.

## Interleaving fan-out - `parallel(..., interleavings=)`

`parallel(fn1, fn2, …)` (Chapter 5) runs branches concurrently. rc2 adds an `interleavings=` kwarg that turns each ordering into a leaf:

```python
def step_a(): api.post("/order")
def step_b(): api.post("/inventory_adjust")
def step_c(): api.post("/audit_log")

def test_concurrent():
    parallel(step_a, step_b, step_c, interleavings = "all")
```

Policies:

| Value | Leaves produced |
|-------|-----------------|
| `1` (default) | One - runs once, branches concurrently as in rc1 |
| `"all"` | `factorial(N)` - every distinct ordering |
| `"critical"` | `min(2N-1, N!)` - head-to-head + sequential pairs heuristic |
| Integer N | `min(N, factorial(branches))` - capped subset, first N in plan order |

For 3 branches with `"all"`, that's 3! = 6 leaves with launch orderings `[a,b,c]`, `[a,c,b]`, `[b,a,c]`, …

### Reserved values - locked for future RFCs

```python
parallel(a, b, interleavings = "dpor")          # → spec-load error, RFC-009
parallel(a, b, interleavings = "sut-internal")  # → spec-load error, L5 / RFC-040 Appendix A
```

These are explicit "future release" rejections so CI integrations gating on the value don't drift when the corresponding RFC lands.

### Scope limit (rc2)

Today's interleaving is **launch ordering** - branches launch sequentially in the per-leaf order. Mediated-event-level interleaving (two branches running concurrently while the engine releases their syscalls in a specific sequence) is a follow-up. The kwarg surface and plan-tree leaf descriptors are in place for that work to plug into; specs you write today stay valid when the engine refines.

## `faultbox plan` - preview the cross-product

Before committing CI time to a test, ask Faultbox what it will run:

```bash
$ faultbox plan checkout.star
Plan tree:
  test_checkout_matrix [27 instances]
      ├── choose: retries = [0, 1, 3]
      ├── choose: fault = [503, 504, timeout]
      └── choose: backoff = [10, 100, 1000]
  test_concurrent_orders [6 instances]
      └── parallel: 3 branches, interleavings = "all" → 6 leaves
  test_wal_robustness [4 instances]
      └── probability: wal (max_fires=2) → 4 leaves
Totals: 37 instances
```

The command runs *only* spec loading + static analysis - no services launched, no Docker pulls, no Lima VM start. Sub-100ms even for large specs.

### `--check-cost --max-instances N`

Use the cost gate in CI / pre-commit hooks to block accidental explosions:

```bash
$ faultbox plan checkout.star --check-cost --max-instances 100
ok: 37 instances within budget of 100
```

```bash
$ faultbox plan checkout.star --check-cost --max-instances 30
error: plan has 37 instances, exceeds budget of 30
exit code: 2
```

Wire into a pre-commit hook on the spec directory and your team gets a hard stop before someone accidentally adds a `choose([0..1000])` axis.

### `--format=json` for tooling

Pipe the plan into `jq` or a dashboard:

```bash
$ faultbox plan checkout.star --format=json | jq '.totals.instances'
37
```

The JSON schema is versioned (`schema_version: 1`); the same shape lives in every bundle's `plan.json` so tooling that reads one reads both.

## Composition - the cross-product

The three axis kinds - `choose`, probability, interleaving - multiply:

```python
def test_everything():
    retries = choose("retries", [0, 1, 3])          # 3
    parallel(a, b, c, interleavings = "all")        # 3! = 6
    fault(db, write = deny("EIO",
        probability = 0.3, max_fires = 2,           # 2² = 4
        label = "wal"))
    api.run()

# Total: 3 × 6 × 4 = 72 leaves
```

The plan walker enumerates the cross-product in mixed-radix order so leaf indices are stable across runs. The bundle's `plan.json` records every axis; the HTML report's tests table shows the per-leaf assignment in the row label:

```
test_everything [leaf 17 - retries=2, parallel#3, wal[01]]
```

## Reading a specific leaf's trace

Each leaf gets its own trace event log. To find leaf 17's events:

1. Open the HTML report (`faultbox report run.fb` produces `report.html`).
2. The tests table shows every leaf as a separate row - sort/filter by name to find your test.
3. Click the leaf row to open the drill-down: per-leaf trace, swim-lane visualization, replay command.

For CLI inspection: `cat run.fb/manifest.json | jq '.tests[] | select(.leaf_id=="17")'` gets the row metadata; `faultbox replay run.fb --test test_everything --leaf 17` (when leaf-selection lands - currently leaves share the test name in `--test`) reruns that specific configuration.

## When to use which axis kind

| Use case | Axis kind |
|----------|-----------|
| Configuration parameter (retry count, timeout value, mode flag) | `choose("name", [opts])` |
| Rare failure mode you want exhaustive coverage of | `probability=p, max_fires=N` |
| Two independent operations that might arrive in any order | `parallel(a, b, interleavings="all")` |
| Combination that doesn't make sense | `halt()` inside body, or `assume=` predicate |

For test design: pick the smallest axis count that exercises the failure modes you care about. `choose("retries", [0, 1])` + `probability=0.3, max_fires=1` is usually more informative than `choose("retries", [0, 1, 2, 3, 4, 5])` - diminishing returns set in fast and the plan grows multiplicatively.

## Exercises

1. **Count before you run.** Write a spec with three `choose()` axes (cardinalities 2, 3, 4) and one `parallel(interleavings="critical")` with 4 branches. Predict the leaf count, then run `faultbox plan --format=json | jq '.totals.instances'` to verify.

2. **Cost gate.** Add the `faultbox plan --check-cost --max-instances 50` invocation to your repo's pre-commit hook. Add an axis that pushes the count over 50 and confirm the hook blocks the commit.

3. **Exhaustive WAL coverage.** Take an existing spec that uses `probability=0.3` on a WAL write. Add `max_fires=3, mode="exhaustive"` and observe the eight-leaf plan tree (2³) in `faultbox plan`. Find the leaf where every WAL write fired - what does the trace look like compared to the all-passed leaf?

4. **Interleaving identity.** Compare `parallel(a, b, interleavings=1)` and `parallel(a, b, interleavings="all")` plans. The first should produce 1 leaf, the second 2. Verify with `faultbox plan`.

## What's next

You've reached the end of Part 5. The full verification toolkit is at your disposal: invariants (Ch 14), monitors (Ch 15), partitions (Ch 16), scenarios & the fault matrix (Ch 10), determinism (Ch 24), operators (Ch 25), and plan-tree fan-out (this chapter). Part 6 covers the power tools - event sources, named operations, LLM agents, bundles, and reports.
