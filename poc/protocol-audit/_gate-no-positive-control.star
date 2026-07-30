# M1 gate, known-bad half: a reconstruction of the shape that hid two credential
# bugs in CI for three releases.
#
# Every assertion here is satisfied by a client that cannot connect at all.
# NO_POSITIVE_CONTROL must fire.
#
# Not part of the audit suite — the leading underscore keeps it out of default
# discovery; it exists to be run explicitly by the M1 gate.

pg = service("pg",
    interface("main", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "faultbox", "POSTGRES_DB": "app"},
    healthcheck = ready(timeout = "90s"),
)


def test_only_asserts_failure():
    """The v0.15.0 CI corpus shape: assert the query fails under a fault."""
    def scenario():
        r = pg.main.query(sql = "SELECT 1")
        assert_true(not r.ok, "expected failed query under injected fault")
    fault(pg.main, error(query = "SELECT*", message = "injected: disk full"), run = scenario)
