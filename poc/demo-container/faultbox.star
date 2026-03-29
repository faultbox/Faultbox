# faultbox.star — Container Demo: Go API + Postgres + Redis
#
# Run:  faultbox test faultbox.star
#
# Prerequisites: Docker running, faultbox-shim built

postgres = service("postgres",
    interface("main", "tcp", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "testdb"},
    healthcheck = tcp("localhost:5432", timeout="60s"),
)

redis = service("redis",
    interface("main", "tcp", 6379),
    image = "redis:7-alpine",
    healthcheck = tcp("localhost:6379", timeout="30s"),
)

api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    env = {
        "PORT": "8080",
        "DATABASE_URL": "postgres://postgres:test@" + postgres.main.internal_addr + "/testdb?sslmode=disable",
        "REDIS_URL": "redis://" + redis.main.internal_addr,
    },
    depends_on = [postgres, redis],
    healthcheck = http("localhost:8080/health", timeout="60s"),
)

# --- Tests ---

def test_happy_path():
    """API health check passes with all services running."""
    resp = api.get(path="/health")
    assert_eq(resp.status, 200)

def test_write_and_read():
    """Write a value to Postgres via API, then read it back."""
    resp = api.post(path="/data?key=hello&value=world")
    assert_eq(resp.status, 200)
    assert_true("stored" in resp.body)

    resp = api.get(path="/data/hello")
    assert_eq(resp.status, 200)
    assert_eq(resp.body, "world")

def test_postgres_write_failure():
    """Postgres write fails with EIO — API returns 503."""
    def scenario():
        resp = api.post(path="/data?key=fail&value=test")
        assert_true(resp.status >= 500, "expected 5xx on DB write failure")
    fault(postgres, write=deny("EIO"), run=scenario)

def test_postgres_write_enospc():
    """Postgres disk full — write should return error."""
    def scenario():
        resp = api.post(path="/data?key=disk-full&value=test")
        assert_true(resp.status >= 500, "expected 5xx on ENOSPC")
    fault(postgres, write=deny("ENOSPC"), run=scenario)
