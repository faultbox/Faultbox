# faultbox.star — Container Demo: host-binary Go API + Postgres + Redis
# (containers).
#
# Run:  faultbox test faultbox.star
#
# Prerequisites:
#   - Docker running, faultbox-shim built
#   - api-svc built and staged at /tmp/api-svc (make demo-build does this)
#
# Why host-binary api: when the api lived inside a container, its env
# `DATABASE_URL` substring `postgres:5432` was rewritten by the proxy
# substitution layer to `host.docker.internal:<host-port>` so faults
# could be injected. On Linux Docker (Lima) `host.docker.internal`
# resolves to the docker0 bridge gateway (172.17.0.1), and proxies
# bind to 127.0.0.1 only — so the api couldn't reach them. Running
# the api on the host fixes the path: it dials 127.0.0.1:<host-port>
# directly. Tracked as a follow-up RFC for the conditional-rewrite
# code fix.

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
    "/tmp/api-svc",
    interface("public", "http", 8080),
    env = {
        "PORT": "8080",
        # internal_addr (= "postgres:5432") gets rewritten to the host-side
        # proxy listener (127.0.0.1:<proxy-port>) by the binary-consumer
        # substitution table, so fault rules on `postgres` actually fire.
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
