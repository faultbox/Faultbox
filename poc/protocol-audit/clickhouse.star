# ClickHouse: every step result asserted, with a password configured.
#
# The stock image ships a passwordless `default` user, so this topology sets
# one — that is what exercises the HTTP basic-auth path added in v0.16.1.
# Without credentials a configured server rejects every statement with
# 516 AUTHENTICATION_FAILED.

ch = service("clickhouse",
    interface("main", "clickhouse", 8123),
    image = "clickhouse/clickhouse-server:24.3-alpine",
    env = {
        "CLICKHOUSE_USER":     "faultbox",
        "CLICKHOUSE_PASSWORD": "faultbox",
        "CLICKHOUSE_DB":       "app",
    },
    healthcheck = ready(timeout = "120s"),
)


def test_exec_and_query():
    r = ch.main.exec(sql = "CREATE TABLE IF NOT EXISTS t (id UInt32, payload String) ENGINE = Memory")
    assert_true(r.ok, "CREATE TABLE failed: %s" % r.error)

    r = ch.main.exec(sql = "INSERT INTO t VALUES (1, 'row-1')")
    assert_true(r.ok, "INSERT failed: %s" % r.error)

    r = ch.main.query(sql = "SELECT count() AS n FROM t")
    assert_true(r.ok, "SELECT failed: %s" % r.error)
    print("count:", r.data)


def test_error_is_reported():
    r = ch.main.query(sql = "SELECT * FROM table_that_does_not_exist")
    assert_true(not r.ok, "a missing table must fail the step, got ok=True")
    assert_true(r.error != "", "a failed step must carry an error message")
    print("expected failure surfaced:", r.error[:70])
