# RFC-022 v0.9.1 validation — fault(read=...).
#
# Before v0.9.1: the shim's SCM_RIGHTS ACK read(2) would be trapped
# by its own filter when read was in the user's fault list → hang.
# After v0.9.1: InstallFilter whitelists the pre-opened socket fd
# for the read family → handoff completes normally.

db = service("db",
    interface("main", "mysql", 3306),
    image = "mysql:8.0.32",
    env = {
        "MYSQL_ROOT_PASSWORD": "test",
        "MYSQL_DATABASE":      "testdb",
    },
    healthcheck = tcp("localhost:3306"),
)

def test_read_fault_does_not_deadlock():
    # read=deny is what a customer writes to simulate "DB returns no
    # data on SELECT" scenarios. Pre-v0.9.1, the shim's own ACK read
    # on the host Unix socket would be trapped → 180s hang.
    fault(db, read=deny("EIO"), run=lambda: assert_true(True))
