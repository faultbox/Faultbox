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
    statuses = [r.status for r in results]
    ok_count = sum(1 for s in statuses if s == 200)
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

Every test run has a seed (default: 0). The seed controls scheduling:

```bash
# Linux:
bin/faultbox test concurrency-test.star --seed 42
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --seed 42
```

**Same seed = same interleaving = same result.** This makes concurrency bugs
**reproducible** — the holy grail of distributed debugging.

When a test fails, the output includes the replay command:
```
--- FAIL: test_concurrent_orders (300ms, seed=7) ---
  replay: faultbox test concurrency-test.star --test concurrent_orders --seed 7
```

Copy-paste to reproduce. Every time.

## Multi-run discovery

Run the same test many times with different seeds to hunt for failures:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test flaky_network --runs 100 --show fail
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test flaky_network --runs 100 --show fail
```

Seeds 0 through 99 are tried. Only failures are shown. If seed 7 fails:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test flaky_network --seed 7
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test flaky_network --seed 7
```

**The workflow:** run 100 times → find failures → replay → debug → fix → re-run 100 times → all pass.

## Exhaustive exploration

For small state spaces, try EVERY possible ordering:

```bash
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_orders --explore=all
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_orders --explore=all
```

```
exploring 20 interleavings for test_concurrent_orders...
  seed 0: PASS  seed 1: PASS  seed 2: PASS  ...
20/20 passed (20 interleavings explored)
```

With 2 concurrent operations making 3 syscalls each, there are
`6! / (3! * 3!) = 20` possible interleavings. `--explore=all` tries each one.

For larger spaces, sample randomly:

```bash
# Linux:
bin/faultbox test concurrency-test.star --explore=sample --runs 500
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --explore=sample --runs 500
```

**When to use which:**
- `--explore=all` — small state spaces (<100 permutations), complete guarantee
- `--explore=sample --runs N` — large state spaces, probabilistic coverage
- `--runs N` (no explore) — just randomize the seed, fastest

## nondet()

Some services make background syscalls (metrics, logging, healthchecks) that
aren't part of your test scenario. These add noise:

```python
def test_concurrent_orders():
    nondet(monitoring_svc)  # exclude from interleaving control
    results = parallel(...)
```

`nondet()` marks a service's syscalls as "don't hold, don't schedule" —
they proceed immediately without interleaving control.

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

## Combining faults with concurrency

The most powerful tests: concurrent operations under failure conditions.

```python
def test_concurrent_under_failure():
    """Concurrent orders while inventory is slow."""
    def scenario():
        results = parallel(
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        )
        for r in results:
            assert_true(r.status in [200, 503], "expected 200 or 503, not hang")
    fault(inventory, write=delay("500ms"), run=scenario)
```

Run it:
```bash
# Linux:
bin/faultbox test concurrency-test.star --test concurrent_under_failure
# macOS (Lima):
vm bin/linux-arm64/faultbox test concurrency-test.star --test concurrent_under_failure
```

```
--- PASS: test_concurrent_under_failure (600ms, seed=0) ---
  fault rule on inventory: write=delay(500ms) → filter:[write,writev,pwrite64]
```

This tests: "when two orders race AND inventory is slow, does the system
still respond correctly?" This is exactly the scenario that causes
production outages.

## What you learned

- `parallel()` runs operations concurrently with controlled interleaving
- Seeds make concurrency bugs reproducible
- `--runs N` discovers failures across many interleavings
- `--explore=all` guarantees complete coverage for small state spaces
- `nondet()` excludes noisy services from scheduling control
- `--virtual-time` makes exhaustive exploration practical
- Faults + concurrency = realistic distributed stress testing

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
