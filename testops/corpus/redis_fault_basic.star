# testops/corpus/redis_fault_basic.star
#
# Mock-only corpus spec exercising Redis proxy-level fault injection.
# Uses the redis mock (miniredis-backed) + the stdlib Redis recipes that
# emit canonical RESP error strings (OOM, LOADING, READONLY).
#
# Verifies:
#   - GET on a seeded key succeeds baseline (resp.ok == True).
#   - Recipe-driven faults surface as RESP errors on the client
#     (resp.ok == False, error message visible).
#   - Key-pattern matching: fault only fires on matching keys.

load("@faultbox/mocks/redis.star", "redis")
load("@faultbox/recipes/redis.star", redis_recipe = "redis")

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 16381),
    state = {
        "user:1":          "alice",
        "counter:hits":    "0",
        "config:readonly": "false",
    },
)

def test_baseline_get():
    """Without faults, GET returns the seeded value via the mock."""
    resp = cache.main.get(key = "user:1")
    assert_true(resp.ok, "expected ok=True on baseline GET")
    assert_true("alice" in resp.body, "expected seeded value for user:1")

def test_oom_on_all_keys():
    """redis.oom() with default key='*' faults every GET."""
    def scenario():
        resp = cache.main.get(key = "user:1")
        assert_true(not resp.ok, "expected ok=False under OOM fault")
        assert_true("OOM" in resp.error, "expected OOM in error message")
    fault(cache.main, redis_recipe.oom(), run = scenario)

def test_loading_selective():
    """redis.loading() scoped to counter:* keys only faults matching keys."""
    def scenario():
        # counter:* key — should see LOADING error.
        resp = cache.main.get(key = "counter:hits")
        assert_true(not resp.ok, "expected ok=False on matching key")
        assert_true("LOADING" in resp.error, "expected LOADING in error")

        # Non-matching key — should succeed.
        resp2 = cache.main.get(key = "user:1")
        assert_true(resp2.ok, "expected ok=True on non-matching key")
        assert_true("alice" in resp2.body)
    fault(cache.main, redis_recipe.loading(key = "counter:*"), run = scenario)

def test_readonly_on_config_keys():
    """READONLY error scoped to config:* keys."""
    def scenario():
        resp = cache.main.get(key = "config:readonly")
        assert_true(not resp.ok, "expected ok=False on config:* key")
        assert_true("READONLY" in resp.error, "expected READONLY in error")
    fault(cache.main, redis_recipe.readonly_replica(key = "config:*"), run = scenario)
