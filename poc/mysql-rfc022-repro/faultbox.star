# RFC-022 Phase 0 step 3 — reproduce the MySQL 8 seccomp-handoff hang
# with the new structured logging in place.
#
# Expected outcome: the test will fail after ~3 minutes (the test ctx
# deadline) because the shim never completes the SCM_RIGHTS handoff.
# The shim stderr (captured in `docker logs faultbox-db`) + host
# debug output should pinpoint which handoff phase stalled.
#
# Run with:
#   sudo /usr/local/bin/faultbox test \
#     /host-home/git/faultbox/faultbox-rfc022/poc/mysql-rfc022-repro/faultbox.star \
#     --debug --log-format=json
#
# The syscall-level fault below is what triggers the shim path
# (SyscallNrs > 0). Proxy-level faults alone would short-circuit to
# launchSimple and we wouldn't reproduce the hang.
#
# Reference: issue #53, RFC-022 docs/rfcs/0022-multi-process-seccomp.md

db = service("db",
    interface("main", "mysql", 3306),
    image = "mysql:8.0.32",
    env = {
        "MYSQL_ROOT_PASSWORD": "test",
        "MYSQL_DATABASE":      "testdb",
    },
    healthcheck = tcp("localhost:3306"),
    # Deliberately DO NOT set seccomp=False — this is the repro.
)

def test_mysql_seccomp_hang():
    # The repro will fail during service startup (before this body runs)
    # because the shim never completes the SCM_RIGHTS handoff. We just
    # need a test function + fault rule to trigger the shim path.
    fault(db, write=deny("EIO"), run=lambda: assert_true(True))
