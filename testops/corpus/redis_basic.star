# testops/corpus/redis_basic.star — Redis mock, seeded state, no faults.
#
# Minimal spec exercising only the Redis mock surface. Purpose: give the
# regression harness a second runnable case so we catch drift in the
# cache/miniredis path independently of the multi-service mock_demo.
#
# Run directly:  faultbox test testops/corpus/redis_basic.star

load("@faultbox/mocks/redis.star", "redis")

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 16380),
    state = {
        "user:1":       "alice",
        "user:2":       "bob",
        "counter:hits": "0",
    },
)

def test_get_seeded_string():
    resp = cache.main.get(key = "user:1")
    assert_eq(resp.status, 0)
    assert_true("alice" in resp.body, "expected seeded value 'alice' for user:1")

def test_get_second_seeded_string():
    resp = cache.main.get(key = "user:2")
    assert_eq(resp.status, 0)
    assert_true("bob" in resp.body, "expected seeded value 'bob' for user:2")

def test_get_seeded_counter():
    resp = cache.main.get(key = "counter:hits")
    assert_eq(resp.status, 0)
    assert_true("0" in resp.body, "expected seeded counter value '0'")
