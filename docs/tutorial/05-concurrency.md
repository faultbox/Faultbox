# Chapter 5: Exploring Concurrency

**Duration:** 25 minutes
**Prerequisites:** [Chapter 0 (Setup)](00-setup.md) completed

## Goals & Purpose

The hardest distributed systems bugs are **concurrency bugs** — they only
appear when operations happen in a specific order, and that order is random
in production.

Consider: two orders arrive at the same instant for the last item in stock.
Does one get confirmed and the other rejected? Do both get confirmed
(overselling)? Does the system deadlock? The answer depends on the exact
interleaving of syscalls — which is different every time.

Traditional testing can't find these bugs because it runs one operation at a
time. Even "concurrent" tests in Go (`t.Parallel()`) don't control the
interleaving — they just hope to get lucky.

**Faultbox's approach:** intercept syscalls and control their ordering.
By holding syscalls and releasing them in specific sequences, Faultbox can
explore every possible interleaving — or replay a specific one.

This chapter teaches you to:
- **Run operations concurrently** and observe different outcomes
- **Reproduce failures** with deterministic seed replay
- **Explore systematically** — try all possible orderings
- **Combine concurrency with faults** — the ultimate stress test

After this chapter, you'll know how to find bugs that only appear under
specific timing — and how to reproduce them reliably.

## Setup

This chapter uses the same two-service system from Chapter 4:

```
orders (HTTP :8080) ──→ inventory (TCP :5432 + WAL file)
```

Create `concurrency-test.star` in the project root:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

inventory = service("inventory", BIN + "/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders", BIN + "/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)
```

Add all test functions from this chapter to this file.

## parallel()

Run multiple operations concurrently:

```python
def test_concurrent_orders():
    """Two orders at once — no double-spend."""
    results = parallel(
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
    )
    ok_count = 0
    for r in results:
        if r.status == 200:
            ok_count += 1
    assert_true(ok_count >= 1, "at least one order should succeed")
```

Run it:
```bash
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_orders
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_orders
```

```
--- PASS: test_concurrent_orders (300ms, seed=0) ---
  syscall trace (128 events)
```

`parallel()` runs the lambdas concurrently. Their syscalls interleave based
on the **seed**.

## Seeds and replay

**The problem:** concurrency bugs depend on ordering. Two concurrent orders
for the last item in stock might work 99% of the time — but with one
specific interleaving, both orders see "1 in stock" before either reserves,
and you oversell.

The **seed** controls which interleaving happens. Different seeds produce
different orderings:

| Seed | What happens |
|------|-------------|
| 0 | Order A checks → A reserves → B checks (0 left) → B rejected ✓ |
| 7 | Order A checks → B checks (both see 1) → both reserve → **oversold** ✗ |
| 42 | Order B checks → B reserves → A checks (0 left) → A rejected ✓ |

The default seed (0) gives you **one** interleaving. That's not enough —
you need to explore many:

```bash
# Run with a specific seed:
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_orders --seed 42
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_orders --seed 42
```

**Same seed = same interleaving = same result.** This makes concurrency bugs
**reproducible** — the holy grail of distributed debugging.

When a test fails, the output includes the replay command:
```
--- FAIL: test_concurrent_orders (300ms, seed=7) ---
  replay: faultbox test concurrency-test.star --test concurrent_orders --seed 7
```

Copy-paste to reproduce. Every time. No more "works on my machine" for
concurrency bugs.

### How seeds control ordering

Faultbox has two levels of concurrency control depending on the mode:

**Default mode** (`--runs N` without `--explore`): The seed controls the
RNG for probabilistic fault decisions (e.g., `deny("EIO", probability="50%")`).
Concurrent operations run as real goroutines — the Go runtime schedules
them, so ordering has natural variation. The seed provides *statistical*
reproducibility: same seed usually gives the same result, but isn't
guaranteed to produce the exact same syscall order.

**Explore mode** (`--explore=all` or `--explore=sample`): Deterministic
control **at the syscall level**. When `parallel()` runs, Faultbox installs
**hold rules** on all services' syscalls. Every syscall pauses and waits
in a queue. An **ExploreScheduler** then releases them one at a time in a
specific permutation order. Each seed/run produces a different permutation —
a different release order — giving a different interleaving.

> **Important:** Faultbox controls the ordering of **syscalls**, not CPU
> instructions. Between syscalls, threads run freely. A pure in-memory
> race (e.g., two goroutines writing to a shared map without locks)
> has no syscall — Faultbox can't see it. But distributed systems bugs
> involve I/O (network, disk, database) — and those all go through
> syscalls. That's the level where interleaving matters.

```
parallel(order_A, order_B)

Seed 0 permutation:     Seed 7 permutation:
  release A.connect       release B.connect
  release A.write         release A.connect
  release B.connect       release B.write
  release B.write         release A.write    ← different order
```

This is how `--explore=all` can guarantee complete coverage: it
enumerates every possible permutation of syscall release order.

## Multi-run discovery

Run the same test many times with different seeds to hunt for failures:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test flaky_network --runs 100 --show fail
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test flaky_network --runs 100 --show fail
```

`--runs 100` runs the test 100 times, each with a different seed
(0, 1, 2, ..., 99). Each seed produces a different interleaving of
concurrent operations. `--show fail` hides passing runs.

When all runs pass, output is compact — one summary line per test:
```
--- PASS: test_flaky_network (100/100 runs) ---
```
When a run fails, you see the full trace detail for the first failure.

If seed 7 fails, replay it:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test flaky_network --seed 7
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test flaky_network --seed 7
```

**The full workflow:**
1. `--runs 100` → discover which seeds trigger failures
2. `--seed 7` → reproduce the exact failure, debug it
3. Fix the code, rebuild
4. `--seed 7` → verify the fix for that specific interleaving
5. `--runs 100` → verify no other interleavings broke → all pass

## Exhaustive exploration

For small state spaces, try EVERY possible ordering:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_orders --explore=all --runs 20
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_orders --explore=all --runs 20
```

```
--- PASS: test_concurrent_orders (20/20 runs) ---

20 passed, 0 failed
```

With 2 concurrent operations making 3 syscalls each, there are
`6! / (3! * 3!) = 20` possible interleavings. `--explore=all` uses
hold-and-release scheduling — each run produces a different permutation.

> **Current limitation:** `--explore=all` requires you to specify `--runs N`
> manually. Pick a number larger than the expected permutation count —
> extra runs just repeat. A future version will auto-calculate the
> permutation count so `--explore=all` alone is enough.

For larger spaces, sample randomly:

```bash
# Linux:
bin/faultbox test concurrency-test.star --explore=sample --runs 500
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --explore=sample --runs 500
```

**When to use which:**
- `--explore=all --runs N` — small state spaces (<100 permutations), deterministic hold-and-release, complete guarantee
- `--explore=sample --runs N` — large state spaces, deterministic hold-and-release, probabilistic coverage
- `--runs N` (no explore) — natural goroutine scheduling with different seeds, fastest

## nondet()

Some services make background syscalls (metrics, logging, healthchecks) that
aren't part of your test scenario. These add noise:

```python
def test_concurrent_orders():
    nondet(monitoring_svc, cache_svc)  # exclude from interleaving control
    results = parallel(...)
```

`nondet()` accepts one or more services and marks their syscalls as
"don't hold, don't schedule" — they proceed immediately without
interleaving control. Pass multiple services in a single call instead
of calling `nondet()` separately for each.

## Virtual time

Delay faults + exhaustive exploration = very slow (each permutation waits
for real delays). Virtual time skips the waits:

```bash
# Linux:
bin/faultbox test concurrency-test.star --virtual-time --explore=all
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --virtual-time --explore=all
```

A test with `delay("2s")` completes in milliseconds. Virtual time advances
a logical clock instead of sleeping.

> **Why not default?** Virtual time skips real delays, so it can't detect
> real timeout bugs (e.g., a 4.9s response with a 5s timeout). Use real
> time for timing accuracy, virtual time for exploration speed.

## Combining faults with concurrency

The most powerful tests: concurrent operations under failure conditions.

```python
def test_concurrent_under_failure():
    """Two orders for ALL stock while inventory is slow — no overselling."""
    def scenario():
        results = parallel(
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":100}'),
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":100}'),
        )
        # Both must complete (no hangs).
        assert_eq(len(results), 2, "both operations must complete")
        # At most one should succeed — there's only 100 in stock.
        ok_count = 0
        for r in results:
            if r.status == 200:
                ok_count += 1
        assert_true(ok_count <= 1, "at most one order should succeed — no overselling")
    fault(inventory, write=delay("500ms", label="slow inventory"), run=scenario)
```

Both orders request all 100 widgets. Only one can win — the other must
see insufficient stock. The `delay("500ms")` on inventory writes makes
the race window wider, increasing the chance of exposing an oversell bug.

Run it:
```bash
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_under_failure
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_under_failure
```

```
--- PASS: test_concurrent_under_failure (600ms, seed=0) ---
  fault rule on inventory: write=delay(500ms) → filter:[write,writev,pwrite64] label="slow inventory"
```

This tests: "when two orders race AND inventory is slow, does the system
still respond correctly?" This is exactly the scenario that causes
production outages.

## Visualizing interleavings with ShiViz

When a concurrency test fails, the trace tells you *what* happened but
not *why* that ordering was wrong. ShiViz (from Chapter 4) shows the
interleaving visually — which is critical for concurrency debugging.

First, add a test that's **designed to fail** under some interleavings.
This test asserts that both concurrent orders succeed — which can't
always be true when both request all stock:

```python
def test_oversell_bug():
    """Deliberately fragile: asserts both orders succeed. Will fail."""
    def scenario():
        results = parallel(
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":100}'),
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":100}'),
        )
        for r in results:
            assert_eq(r.status, 200, "both orders should succeed")
    fault(inventory, write=delay("100ms", label="race window"), run=scenario)
```

Run it with multiple seeds to find a failure:
```bash
# Linux:
bin/faultbox test concurrency-test.star --test oversell_bug --runs 10
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test oversell_bug --runs 10
```

One of the seeds will fail (the second order gets "insufficient stock").
Note which seed, then capture ShiViz output for both a passing and
failing seed:

**Step 1:** Capture the failing interleaving:
```bash
# Replace --seed 3 with your failing seed:
# Linux:
bin/faultbox test concurrency-test.star --test oversell_bug --seed 3 --shiviz fail.shiviz
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test oversell_bug --seed 3 --shiviz fail.shiviz
```

**Step 2:** Capture a passing interleaving:
```bash
# Linux:
bin/faultbox test concurrency-test.star --test oversell_bug --seed 0 --shiviz pass.shiviz
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test oversell_bug --seed 0 --shiviz pass.shiviz
```

**Step 3:** Open both at https://bestchai.bitbucket.io/shiviz/ and compare.

You'll see swimlanes for each service plus a **"test"** swimlane for
the test driver. Each event now shows rich metadata:
- **Labels** like `[race window]` on faulted syscalls
- **Causal arrows** between services showing request/response flow
- **VIOLATION marker** in the failing trace — a `VIOLATION [test_oversell_bug]`
  event appears at the failure point, making it easy to spot

In the **passing** run, both RESERVE calls happen before inventory
checks stock — the mutex serializes them and both see enough stock.

In the **failing** run, the second RESERVE arrives after the first
drained all stock — it gets "insufficient_stock" and the assertion fails.
The VIOLATION marker shows exactly which assertion failed and why.

**This is the core debugging workflow:** find a failure with `--runs`,
visualize it with `--shiviz`, compare with a passing seed, understand
the ordering, fix the code, replay to verify.

## What you learned

- `parallel()` runs operations concurrently with controlled interleaving
- Seeds make concurrency bugs reproducible
- `--runs N` discovers failures — compact output shows one summary line per test
- `--explore=all` guarantees complete coverage for small state spaces
- `nondet(svc1, svc2, ...)` excludes noisy services from scheduling control
- `--virtual-time` makes exhaustive exploration practical
- ShiViz shows causal arrows, fault labels, and VIOLATION markers for debugging
- Faults + concurrency = realistic distributed stress testing
- ShiViz visualizes interleavings — compare failing vs passing seeds to see the bug

## What's next

You can now find and reproduce concurrency bugs. But some properties should
hold across ALL test scenarios — not just specific ones. "No order should be
confirmed when inventory is unreachable" is a **safety property** that must
always be true.

Chapter 6 introduces monitors (continuous invariant checkers) and network
partitions (targeted connectivity failures between specific services).

## Exercises

1. **Find a race**: Write a test with two concurrent orders for limited
   stock. Run with `--runs 50`. Do any fail? Which seed?

2. **Exhaustive check**: Run the same test with `--explore=all`. How many
   permutations? Do all pass?

3. **Fault + concurrency**: Two concurrent orders while inventory has a
   200ms write delay. Run with `--runs 20`. Does the system still behave?

4. **Virtual time**: Run exercise 3 with `--virtual-time`. Compare wall-clock
   times. How much faster?
