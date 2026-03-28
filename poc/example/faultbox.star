# faultbox.star — Faultbox configuration and tests in Starlark
#
# Run all tests:   faultbox test faultbox.star
# Run one test:    faultbox test faultbox.star --test happy_path

# --- Topology ---

db = service("db",
    "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api",
    "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

# --- Tests ---

def test_happy_path():
    """Normal operation — API and DB both healthy."""
    # TCP: verify DB responds to PING
    resp = db.main.send(data="PING")
    assert_eq(resp, "PONG")

    # HTTP: write and read through API
    resp = api.post(path="/data/testkey", body="hello")
    assert_eq(resp.status, 200)
    assert_true("stored" in resp.body, "expected 'stored' in body")

    resp = api.get(path="/data/testkey")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "hello")

def test_db_slow():
    """DB writes delayed 500ms — API should still work."""
    def scenario():
        resp = api.post(path="/data/slowkey", body="value1")
        assert_eq(resp.status, 200)
        assert_true(resp.duration_ms > 400, "expected delay > 400ms")

        resp = api.get(path="/data/slowkey")
        assert_eq(resp.status, 200)
        assert_eq(resp.body, "value1")

    fault(db, write=delay("500ms"), run=scenario)

def test_api_cannot_reach_db():
    """API connections to DB refused — returns 500."""
    def scenario():
        resp = api.post(path="/data/failkey", body="value1")
        assert_eq(resp.status, 500)
        assert_true("db error" in resp.body, "expected 'db error' in body")

    fault(api, connect=deny("ECONNREFUSED"), run=scenario)
