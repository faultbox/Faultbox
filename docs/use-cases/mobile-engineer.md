# Use Case: Mobile Engineer

**Persona:** Priya, iOS engineer at a healthtech company

## The Problem

Priya's app talks to a BFF (backend-for-frontend) that aggregates data
from 3 microservices. Users report "infinite loading" when on bad network —
the app doesn't handle partial API failures because nobody documented what
the BFF returns when a downstream service is down.

The backend team says "it returns an error" but Priya needs to know the
exact JSON shape to build the right loading states.

## Day 1 with Faultbox

Priya doesn't test the app directly — she tests the BFF's behavior under
failure so she knows what responses to expect:

```python
bff = service("bff",
    interface("public", "http", 8080),
    image="company/bff:latest",
    healthcheck=http("localhost:8080/health"),
)

users_svc = service("users",
    interface("main", "http", 8081),
    image="company/users:latest",
    healthcheck=http("localhost:8081/health"),
)

records_svc = service("records",
    interface("main", "http", 8082),
    image="company/records:latest",
    healthcheck=http("localhost:8082/health"),
)

def test_partial_failure_response_shape():
    """When records service is down, BFF should return user data with records=null."""
    def records_down():
        resp = bff.get(path="/patient/123/dashboard")
        assert_eq(resp.status, 200, "BFF should return 200 even with partial failure")
        assert_true(resp.data["user"] != None, "user data should be present")
        assert_eq(resp.data["records"], None, "records should be null, not error")
        assert_true("error" not in resp.body, "no error leaking to client")
    fault(records_svc, connect=deny("ECONNREFUSED", label="records down"), run=records_down)

def test_slow_bff_returns_within_timeout():
    """BFF should timeout and return partial data within 3s."""
    def slow_users():
        resp = bff.get(path="/patient/123/dashboard")
        assert_true(resp.duration_ms < 3000, "BFF should not wait forever")
        assert_eq(resp.status, 200)
    fault(users_svc, connect=delay("10s", label="users slow"), run=slow_users)

def test_all_services_down():
    """When everything is down, BFF should return 503 with retry-after."""
    def everything_broken():
        resp = bff.get(path="/patient/123/dashboard")
        assert_eq(resp.status, 503)
        assert_true("retry" in resp.body.lower() or resp.data.get("retry_after") != None,
            "should tell client when to retry")
    fault(users_svc, connect=deny("ECONNREFUSED"), run=lambda:
        fault(records_svc, connect=deny("ECONNREFUSED"), run=everything_broken))
```

## What She Gets

A contract for how the BFF behaves under failure — she knows exactly what
JSON shape her app will receive when services are degraded. She codes the
iOS loading states to match.

## Growth Path

- **Week 1:** Adds tests for every "infinite loading" user report — each
  becomes a Faultbox spec that reproduces the exact failure mode.
- **Week 2:** Backend team fixes the BFF based on her specs. Faultbox
  verifies each fix.
- **Month 1:** Uses `print(resp.data)` during development to discover
  response shapes, then turns discoveries into assertions.
- **Month 2:** Shares specs with the Android team — same failure contracts,
  same expectations.

## Key Value

Priya uses Faultbox as a **contract testing tool** — she doesn't test her
app, she tests the backend's promises. When the BFF says "we handle partial
failures gracefully," she has specs that prove it. The "infinite loading"
reports stop.
