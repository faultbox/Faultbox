# Chapter 5: Exploring Concurrency

The hardest bugs happen when things run at the same time. In this chapter
you'll explore concurrent interleavings and find bugs that only appear
under specific timing.

## The problem

Two orders arrive simultaneously for the last widget in stock. Which wins?
Do both succeed (overselling)? Does one fail gracefully? Does the system crash?

The answer depends on the *order* in which syscalls execute — and in production,
that order is random.

## parallel()

Run multiple operations concurrently:

```python
def test_concurrent_orders():
    """Two orders at once — no double-spend."""
    results = parallel(
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
    )
    # At most one should succeed (stock=10, but in a race condition test
    # with stock=1, exactly one should fail).
    statuses = [r.status for r in results]
    ok_count = sum(1 for s in statuses if s == 200)
    assert_true(ok_count >= 1, "at least one order should succeed")
```

`parallel()` runs the lambdas in concurrent goroutines. The interleaving of
their syscalls depends on the **seed**.

## Seed-based replay

Every test run has a seed (default: 0). The seed controls the scheduling
of held syscalls in `parallel()`:

```bash
faultbox test poc/demo/faultbox.star --seed 42
```

Same seed = same interleaving = same result. This makes concurrent bugs
**reproducible**.

When a test fails, the output includes a replay command:
```
--- FAIL: test_concurrent_orders (300ms, seed=7) ---
  replay: faultbox test poc/demo/faultbox.star --test concurrent_orders --seed 7
```

## Multi-run discovery

Run the same test many times with different seeds to find failures:

```bash
faultbox test poc/demo/faultbox.star --test flaky_network --runs 100 --show fail
```

This runs the test 100 times (seeds 0-99) and only shows failures.
If seed 7 fails, you can replay it:

```bash
faultbox test poc/demo/faultbox.star --test flaky_network --seed 7
```

## Exhaustive exploration

For small numbers of concurrent operations, try ALL possible orderings:

```bash
faultbox test poc/demo/faultbox.star --test concurrent_orders --explore=all
```

With 2 concurrent operations that each make 3 syscalls, there are
`6! / (3! * 3!) = 20` possible interleavings. `--explore=all` tries every one.

For larger state spaces, sample randomly:

```bash
faultbox test poc/demo/faultbox.star --explore=sample --runs 500
```

## nondet()

Some services make background syscalls (healthchecks, metrics, logging) that
aren't part of the test scenario. These add noise to interleaving exploration.

Exclude them:

```python
def test_concurrent_orders():
    nondet(monitoring_svc)  # exclude from ordering exploration
    results = parallel(
        lambda: orders.post(...),
        lambda: orders.post(...),
    )
```

## Virtual time

When faults use `delay()`, real wall-clock sleeps make exhaustive exploration
slow. Virtual time skips the sleeps:

```bash
faultbox test poc/demo/faultbox.star --virtual-time --explore=all
```

A test with `delay("2s")` completes in milliseconds.

## Combining faults with concurrency

The most powerful tests combine fault injection with concurrent operations:

```python
def test_concurrent_under_failure():
    """Concurrent orders while inventory is slow."""
    def scenario():
        results = parallel(
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
            lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        )
        # Both should eventually get a response (not hang).
        for r in results:
            assert_true(r.status in [200, 503], "expected 200 or 503")
    fault(inventory, write=delay("500ms"), run=scenario)
```

## What you learned

- `parallel()` runs operations concurrently
- Seeds control interleaving — same seed = same result
- `--runs 100 --show fail` finds flaky failures
- `--seed N` replays a specific interleaving
- `--explore=all` tries every permutation
- `nondet()` excludes services from ordering
- `--virtual-time` skips delays for fast exploration
- Faults + concurrency = realistic distributed systems testing

## Exercises

1. **Find the race**: Write a test with two concurrent orders for a
   limited-stock item. Run with `--runs 50`. Do any fail? If so, what seed?

2. **Exhaustive check**: Take the same test and run with `--explore=all`.
   How many permutations are tested? Do all pass?

3. **Fault + concurrency**: Write a test where two orders run concurrently
   while inventory has a 200ms write delay. Does the system still work?
   Run with `--runs 20` to check.

4. **Virtual time speedup**: Run exercise 3 with `--virtual-time`. How much
   faster is it? Compare wall-clock times.
