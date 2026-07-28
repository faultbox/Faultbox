# Link shapers: bandwidth() and mtu() (RFC-054, v0.14.1).
#
#     sudo faultbox test poc/gvisor-rfc054/shapers.star
#
# Requires Linux with CAP_NET_ADMIN. On macOS: limactl shell faultbox-dev.
#
# These are the two v0.14.0 deferrals. Unlike every packet_* builtin they take
# no matcher, because they describe the link rather than any packet crossing
# it.
#
# WHAT THESE TESTS ASSERT, AND WHAT THEY DELIBERATELY DO NOT
#
# Each test asserts two things: the shaper was applied (a bandwidth_applied /
# mtu_applied event exists) and traffic still flows through it. Neither is a
# timing assertion. Faultbox has no clock primitive a spec can read, and a
# wall-clock threshold in a test is really an assertion about how loaded the
# machine was — the lesson from the Raft snapshot false positive
# (docs/design/2026-07-28-raft-mesh-results.md §4.0).
#
# The quantitative behaviour — that a 100-byte packet at 1000 B/s waits exactly
# 100ms, that the queue drops when it exceeds its backlog bound — is verified
# in internal/netfault/shape_test.go against a fake clock, where the numbers
# are exact and the test cannot flake.
#
# "The link still carries traffic" is the assertion that matters here, because
# the failure mode being guarded against is a shaper that black-holes the
# connection instead of slowing it.

determinism(runtime = "gvisor")

db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)


def hit_api(n = 6):
    """Drive real api → db traffic across the gateway.

    Only /data/ dials the DB; a path that never reaches it would leave the
    shaper with nothing to shape while the test still passed.
    """
    ok = 0
    for i in range(n):
        api.post(path = "/data/k%d" % i, body = "value-%d" % i)
        if api.get(path = "/data/k%d" % i).status == 200:
            ok += 1
    return ok


def applied(kind):
    return len(events(where = lambda e: e.type == kind))


# --- Baseline ---------------------------------------------------------

def test_unshaped_baseline():
    """No shaper — the comparison point for the two below."""
    assert_true(hit_api() > 0, "baseline traffic did not reach the DB")
    assert_true(applied("bandwidth_applied") == 0,
        "no bandwidth() was called, but a bandwidth_applied event exists")


# --- bandwidth() ------------------------------------------------------

def test_bandwidth_slows_without_breaking():
    """A 64kbit link must still carry the workload.

    The failure being guarded against is a shaper that discards instead of
    paces — a link that is "slow" by virtue of dropping everything.
    """
    bandwidth(rate = "64kbit")
    assert_true(applied("bandwidth_applied") == 1,
        "bandwidth() did not record that it was applied")
    assert_true(hit_api() > 0,
        "no request completed under a 64kbit shaper — the link is dropping, not pacing")


def test_bandwidth_one_direction():
    """dir= shapes one leg only; the reverse path stays at full speed."""
    bandwidth(rate = "128kbit", dir = "s2c")
    assert_true(hit_api() > 0, "traffic stopped under a one-way shaper")


# --- mtu() ------------------------------------------------------------

def test_mtu_shrinks_segments_without_loss():
    """A 576-byte path must still carry traffic.

    This is the distinction from v0.14.0's packet_drop(len_gt=576)
    approximation. That dropped oversized packets, which looks like a black
    hole and behaves like nothing real; a genuine small-MTU path makes TCP
    negotiate a smaller MSS, so the request still succeeds.
    """
    mtu(size = 576)
    assert_true(applied("mtu_applied") == 1, "mtu() did not record that it was applied")
    assert_true(hit_api() > 0,
        "no request completed under mtu=576 — a small MTU should shrink segments, not drop them")


def test_shapers_compose():
    """Both at once: a slow, small-MTU path is a normal thing to model."""
    bandwidth(rate = "256kbit")
    mtu(size = 1200)
    assert_true(hit_api() > 0, "traffic stopped when both shapers were active")
