# testops/corpus/parallel_basic.star
#
# Mock-only spec exercising the parallel() builtin. Runs two concurrent
# step calls against a redis mock; both must observe the same seeded
# state. Under the hood this exercises the runtime's hold-and-release
# scheduler setup/teardown (see builtinParallel). With
# --explore=all/sample the scheduler additionally enumerates
# interleavings; we don't use --explore here so the run is
# deterministic under --seed 1.

load("@faultbox/mocks/redis.star", "redis")

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 16384),
    state = {
        "a": "alpha",
        "b": "beta",
    },
)

def test_parallel_reads_both_succeed():
    """Two concurrent reads return their seeded values."""
    results = parallel(
        lambda: cache.main.get(key = "a"),
        lambda: cache.main.get(key = "b"),
    )
    assert_eq(len(results), 2)
    for r in results:
        assert_true(r.ok, "concurrent read succeeded")
    # Results are in argument order (parallel() contract).
    assert_true("alpha" in results[0].body, "first result is a=alpha")
    assert_true("beta" in results[1].body, "second result is b=beta")

def test_parallel_same_key_idempotent():
    """Two concurrent reads of the same key agree."""
    results = parallel(
        lambda: cache.main.get(key = "a"),
        lambda: cache.main.get(key = "a"),
    )
    assert_eq(len(results), 2)
    assert_true(results[0].ok)
    assert_true(results[1].ok)
    assert_eq(results[0].body, results[1].body)
