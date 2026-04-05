# Use Case: QA Engineer

**Persona:** Marcus, QA lead at an e-commerce company

## The Problem

Marcus's team has 200 integration tests in Selenium, but zero tests for
"what happens when Redis is down" or "what if the search service is slow."
Every Black Friday, something breaks that wasn't tested.

The engineering team says "we handle errors" but nobody has verified that
claim under realistic failure conditions.

## Day 1 with Faultbox

Marcus doesn't write Go — he writes Starlark specs that exercise existing
Docker images without any code changes:

```python
api = service("api",
    interface("public", "http", 8080),
    image="company/api:latest",
    healthcheck=http("localhost:8080/health"),
)

redis = service("redis",
    interface("main", "redis", 6379),
    image="redis:7",
    healthcheck=tcp("localhost:6379"),
)

search = service("search",
    interface("main", "http", 9200),
    image="company/search:latest",
    healthcheck=http("localhost:9200/health"),
)

def test_checkout_when_redis_down():
    """Cart still works if Redis cache is gone — falls back to Postgres."""
    api.post(path="/cart/add", body='{"sku": "TSHIRT", "qty": 1}')

    def redis_dies():
        resp = api.post(path="/checkout", body='{"payment": "card"}')
        assert_true(resp.status == 200 or resp.status == 503,
            "checkout should succeed (DB fallback) or fail gracefully, not 500")
    fault(redis, connect=deny("ECONNREFUSED", label="Redis down"), run=redis_dies)

def test_search_timeout_doesnt_block_checkout():
    """Slow search shouldn't cascade to checkout."""
    def slow_search():
        resp = api.post(path="/checkout", body='{"payment": "card"}')
        assert_true(resp.duration_ms < 5000, "checkout shouldn't wait for search")
    fault(search, connect=delay("10s", label="search timeout"), run=slow_search)

def test_full_disk_error_message():
    """When disk is full, API should return a useful error, not 'internal error'."""
    def disk_full():
        resp = api.post(path="/orders", body='{"item": "WIDGET"}')
        assert_true(resp.status >= 500, "expected 5xx")
        assert_true("disk" in resp.body or "space" in resp.body,
            "error message should mention disk/space, not generic error")
    fault(api, write=deny("ENOSPC", label="disk full"), run=disk_full)
```

## What He Gets

Failure mode coverage for Black Friday scenarios. No code changes needed —
he tests existing containers as-is. He adds 15 failure scenarios in a week.

## Growth Path

- **Week 1:** Writes tests for every dependency × every failure mode (the
  "chaos matrix").
- **Week 2:** Adds `--runs 100` nightly runs to catch intermittent failures
  under different interleavings.
- **Month 1:** When a test fails, he files a ticket with the ShiViz trace
  attached — engineers see exactly what happened without reproducing.
- **Month 2:** Builds a dashboard of failure mode coverage per service.

## Key Value

Marcus is the **fastest adopter** — he doesn't need to change code, just
write specs against existing images. He turns "we think we handle errors"
into "we verified we handle these 47 failure modes."
