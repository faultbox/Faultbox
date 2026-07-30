# Postgres: the control for this audit.
#
# The credential and auth-relay bugs were fixed in v0.16.0; this spec is what
# proves they stay fixed, and it is the shape every protocol audit follows —
# real server, real statements, every result asserted.

db = service("pg",
    interface("main", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {
        "POSTGRES_PASSWORD": "faultbox",
        "POSTGRES_DB":       "app",
    },
    healthcheck = ready(timeout = "90s"),
)


def test_exec_and_query():
    r = db.main.exec(sql = "CREATE TABLE IF NOT EXISTS t (id int, payload text)")
    assert_true(r.ok, "CREATE TABLE failed: %s" % r.error)

    r = db.main.exec(sql = "INSERT INTO t VALUES (1, 'row-1')")
    assert_true(r.ok, "INSERT failed: %s" % r.error)

    r = db.main.query(sql = "SELECT count(*) AS n FROM t")
    assert_true(r.ok, "SELECT failed: %s" % r.error)
    assert_true(len(r.data) == 1, "expected one row, got %s" % r.data)
    print("count:", r.data)


def test_error_is_reported_not_swallowed():
    """A genuinely bad statement must come back as ok=False, not silently."""
    r = db.main.query(sql = "SELECT * FROM table_that_does_not_exist")
    assert_true(not r.ok, "a missing table must fail the step, got ok=True")
    assert_true(r.error != "", "a failed step must carry an error message")
    print("expected failure surfaced:", r.error[:60])
