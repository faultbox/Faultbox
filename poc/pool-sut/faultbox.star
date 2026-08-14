# faultbox.star — F-1 regression corpus.
#
# Reproduces the conditions that used to kill the seccomp supervisor: a
# SUT with a pool of connections, all of them making intercepted syscalls
# concurrently, running long enough for a notification to be dropped.
#
# Before the fix, an ENOENT from SECCOMP_IOCTL_NOTIF_RECV was classified
# as a closed listener fd. The notification loop returned nil, silently,
# and the SUT kept its filter with nobody answering it — so every
# intercepted syscall blocked forever. The visible symptom was every
# socket timing out at once and never recovering.
#
# Build and run (Lima):
#   GOOS=linux GOARCH=arm64 go build -o /tmp/pool-sut  ./poc/pool-sut/
#   GOOS=linux GOARCH=arm64 go build -o /tmp/mock-db   ./poc/mock-db/
#   sudo faultbox test poc/pool-sut/faultbox.star

db = service("db",
    "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

# POOL_SIZE is what makes this test meaningful. A single-connection SUT
# almost never drops a notification; 24 concurrent workers under Go's
# SIGURG preemption reliably do.
sut = service("sut",
    "/tmp/pool-sut",
    interface("public", "http", 8080),
    env = {
        "PORT": "8080",
        "UPSTREAM_ADDR": db.main.addr,
        "POOL_SIZE": "24",
    },
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)


def _stats():
    """Read the SUT's own counters."""
    resp = sut.get(path = "/stats")
    assert_eq(resp.status, 200, "SUT should still be serving")
    return resp.data


def test_pool_survives_a_supervised_run():
    """The pool keeps working while its syscalls are supervised.

    The fault is a no-op by design: `delay("1ms")` on write installs a
    filter and routes every write through the notification loop without
    changing behaviour. What is under test is the supervisor's ability to
    keep answering, not the fault itself.
    """

    def scenario():
        before = _stats()

        # Long enough for the race to land. The original bug showed up
        # within seconds under this load.
        sleep("20s")

        after = _stats()

        progressed = after["round_trips"] - before["round_trips"]
        assert_true(
            progressed > 0,
            "pool made no progress under supervision (%d round trips) — " % progressed +
            "the supervisor most likely stopped answering",
        )

        # The regression signal. A stalled round trip is one that took
        # longer than 5s, which is what every in-flight request does the
        # instant the supervisor dies.
        assert_eq(
            after["stalls"], 0,
            "pool stalled: intercepted syscalls stopped being answered",
        )

    fault(sut, write = delay("1ms"), run = scenario)


def test_pool_still_works_after_the_fault_is_removed():
    """A run that survived supervision keeps working once it ends.

    Guards the teardown half: stopping the loop must not leave the SUT
    filtered and unattended either.
    """

    def scenario():
        # Long enough for the pool to issue writes through the filter.
        # An empty body would leave the fault window too short to fire,
        # and a corpus spec that always warns teaches people to ignore
        # warnings.
        sleep("2s")

    fault(sut, write = delay("1ms"), run = scenario)

    after = _stats()
    assert_true(after["round_trips"] > 0, "pool never made progress")
    assert_eq(after["stalls"], 0, "pool stalled after the fault was removed")
