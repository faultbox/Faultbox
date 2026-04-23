# testops/corpus/postgres_fault_basic.star
#
# LinuxOnly corpus spec covering Postgres proxy-level fault injection
# against a real postgres:16-alpine container.
#
# The fault rule matches on SQL text (query="SELECT*") and is handled
# by faultbox's proxy BEFORE the request reaches postgres — so this
# test doesn't depend on authentication round-tripping.

pg = service("pg",
    interface("main", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {
        "POSTGRES_HOST_AUTH_METHOD": "trust",
        "POSTGRES_USER":              "postgres",
        "POSTGRES_DB":                "postgres",
    },
    healthcheck = tcp("localhost:5432", timeout = "60s"),
)

def test_proxy_fault_rewrites_select():
    """error(query='SELECT*', ...) causes the proxy to close the SQL round-trip.

    The exact client-visible shape depends on the proxy's postgres wire
    implementation (the empirical behavior is an EOF), but the
    observable contract is: a SELECT that would have succeeded against
    the real backend comes back as a failure. That's what matters for
    "proxy fault injection reached the client".
    """
    def scenario():
        resp = pg.main.query(sql = "SELECT 1")
        assert_true(not resp.ok, "expected failed query under injected fault")
        assert_true(resp.error != "", "expected non-empty error")
    fault(pg.main, error(query = "SELECT*", message = "injected: disk full"), run = scenario)
