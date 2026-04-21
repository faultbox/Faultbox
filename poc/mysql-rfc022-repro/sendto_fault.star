# RFC-022 v0.9.1 validation — fault(sendto=...).
#
# The sendto family in faultbox expands to {sendto, sendmsg} (see
# expandSyscallFamily in internal/star/runtime.go). Before v0.9.1,
# the shim's WriteMsgUnix(SCM_RIGHTS) call used sendmsg(2) which
# would be trapped by its own filter when sendto was in the user's
# fault list → hang. After v0.9.1: InstallFilter whitelists the
# pre-opened socket fd for the sendmsg/sendto/recvmsg/recvfrom
# family → handoff completes normally.

db = service("db",
    interface("main", "mysql", 3306),
    image = "mysql:8.0.32",
    env = {
        "MYSQL_ROOT_PASSWORD": "test",
        "MYSQL_DATABASE":      "testdb",
    },
    healthcheck = tcp("localhost:3306"),
)

def test_sendto_fault_does_not_deadlock():
    # sendto=deny with sendmsg family-expansion is the second class
    # of self-deadlock we preemptively fixed in v0.9.1.
    fault(db, sendto=deny("ECONNREFUSED"), run=lambda: assert_true(True))
