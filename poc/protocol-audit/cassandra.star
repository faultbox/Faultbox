# Cassandra: every step result asserted.
#
# The stock image uses AllowAllAuthenticator, so credentials are optional here;
# this spec covers the step path and the ready() check, both of which were
# broken before v0.16.1 (ready() resolved to a URL the gocql path could not
# parse and dialled "cassandra://localhost" literally).
#
# Cassandra is slow to become available — the timeout is generous on purpose.

cass = service("cassandra",
    interface("main", "cassandra", 9042),
    image = "cassandra:4.1",
    env = {"CASSANDRA_CLUSTER_NAME": "faultbox"},
    healthcheck = ready(timeout = "300s"),
)


def test_keyspace_and_table():
    r = cass.main.exec(cql = "CREATE KEYSPACE IF NOT EXISTS app " +
        "WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}")
    assert_true(r.ok, "CREATE KEYSPACE failed: %s" % r.error)

    r = cass.main.exec(cql = "CREATE TABLE IF NOT EXISTS app.t (id int PRIMARY KEY, payload text)")
    assert_true(r.ok, "CREATE TABLE failed: %s" % r.error)

    r = cass.main.exec(cql = "INSERT INTO app.t (id, payload) VALUES (1, 'row-1')")
    assert_true(r.ok, "INSERT failed: %s" % r.error)

    r = cass.main.query(cql = "SELECT count(*) AS n FROM app.t")
    assert_true(r.ok, "SELECT failed: %s" % r.error)
    print("count:", r.data)


def test_error_is_reported():
    r = cass.main.query(cql = "SELECT * FROM app.table_that_does_not_exist")
    assert_true(not r.ok, "a missing table must fail the step, got ok=True")
    assert_true(r.error != "", "a failed step must carry an error message")
    print("expected failure surfaced:", r.error[:70])
