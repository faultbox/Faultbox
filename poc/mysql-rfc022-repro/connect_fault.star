# RFC-022 Phase 1 validation — customer-pattern repro.
#
# Customer report (issue #51): using `connect=deny(...)` on a Docker
# service causes the shim's own net.Dial to the host Unix socket to
# be trapped by its own filter → shim hangs 180s at dial_socket.
#
# Pre-Phase-1 (v0.8.7): this spec hangs for the full test timeout
# because connect is in the filter and the shim dials AFTER filter
# install.
#
# Post-Phase-1 (v0.8.8): connect happens BEFORE filter install, so
# the dial completes immediately and the handoff proceeds normally.
# Healthcheck should succeed within a few hundred ms.
#
# Run: sudo /usr/local/bin/faultbox test \
#        /host-home/git/faultbox/faultbox/poc/mysql-rfc022-repro/connect_fault.star \
#        --debug --log-format=json

db = service("db",
    interface("main", "mysql", 3306),
    image = "mysql:8.0.32",
    env = {
        "MYSQL_ROOT_PASSWORD": "test",
        "MYSQL_DATABASE":      "testdb",
    },
    healthcheck = tcp("localhost:3306"),
)

def test_connect_fault_does_not_deadlock():
    # The fault rule targets `connect` — exactly the customer's pattern.
    # Before Phase 1: shim's own Dial trapped by this filter → 180s hang.
    # After Phase 1: Dial happens pre-filter → handoff completes fast.
    fault(db, connect=deny("ECONNREFUSED"), run=lambda: assert_true(True))
