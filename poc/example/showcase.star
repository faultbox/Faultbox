# showcase.star - the spec behind the site's sample report
# (faultbox-site/public/sample-report.html). Regenerate with:
#
#   faultbox test showcase.star        # in the Lima VM, binaries in /tmp
#   faultbox report <bundle.fb> -o ../../../faultbox-site/public/sample-report.html
#
# The suite deliberately encodes a requirement mock-api does not meet -
# "reads must survive a cache outage" - so the report showcases a real
# found bug: expected 200, got a raw 500.

cache = service("cache",
    "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api",
    "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": cache.main.addr},
    depends_on = [cache],
    healthcheck = http("localhost:8080/health"),
)

def test_happy_path():
    """Write and read through the API - everything healthy."""
    resp = api.post(path="/data/greeting", body="hello")
    assert_eq(resp.status, 200)

    resp = api.get(path="/data/greeting")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "hello")

def test_slow_cache_within_deadline():
    """Cache 300ms slower - requests must still complete fine."""
    def scenario():
        resp = api.post(path="/data/slowkey", body="v")
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 250, "expected the delay to be visible")

    fault(cache, write=delay("300ms"), run=scenario)

def test_reads_survive_cache_outage():
    """A cache outage must not take reads down with it."""
    api.post(path="/data/greeting", body="hello")  # warm the key

    def scenario():
        resp = api.get(path="/data/greeting")
        assert_eq(resp.status, 200,
            "reads must survive a cache outage - degrade, don't 500")

    fault(api, connect=deny("ECONNREFUSED"), run=scenario)
