# The corpus pattern from RFC-056, applied to MySQL.
#
#     faultbox test poc/protocol-audit/mysql.star
#
# The postgres bug (v0.16.0) survived because no spec ever asserted on the
# result of a postgres step. This spec asserts on every MySQL step, against
# the exact configuration docs/protocols/mysql.md tells people to write.

db = service("mysql",
    interface("main", "mysql", 3306),
    image = "mysql:8.0.32",
    env = {
        "MYSQL_ROOT_PASSWORD": "faultbox",
        "MYSQL_DATABASE":      "app",
    },
    healthcheck = ready(timeout = "180s"),
)


def test_exec_is_checked():
    """A DDL statement must actually run."""
    r = db.main.exec(sql = "CREATE TABLE IF NOT EXISTS t (id INT, payload VARCHAR(64))")
    assert_true(r.ok, "CREATE TABLE failed: %s" % r.error)


def test_insert_and_read_back():
    """Write, then prove the write is there."""
    r = db.main.exec(sql = "CREATE TABLE IF NOT EXISTS t (id INT, payload VARCHAR(64))")
    assert_true(r.ok, "CREATE TABLE failed: %s" % r.error)

    r = db.main.exec(sql = "INSERT INTO t VALUES (1, 'row-1')")
    assert_true(r.ok, "INSERT failed: %s" % r.error)

    r = db.main.query(sql = "SELECT COUNT(*) AS n FROM t")
    assert_true(r.ok, "SELECT failed: %s" % r.error)
    assert_true(len(r.data) == 1, "expected one row, got %s" % r.data)
