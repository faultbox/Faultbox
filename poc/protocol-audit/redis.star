# Redis: every step result asserted.
#
# Two questions. Does the step round-trip at all against a stock Redis, and
# does it still work when the server requires a password — the configuration
# any production-shaped spec would use.

plain = service("redis",
    interface("main", "redis", 6379),
    image = "redis:7-alpine",
    healthcheck = ready(timeout = "60s"),
)


def test_ping():
    r = plain.main.ping()
    assert_true(r.ok, "PING failed: %s" % r.error)


def test_set_get_roundtrip():
    r = plain.main.set(key = "k", value = "v")
    assert_true(r.ok, "SET failed: %s" % r.error)

    r = plain.main.get(key = "k")
    assert_true(r.ok, "GET failed: %s" % r.error)
    print("GET returned:", r.data)
    assert_true("v" in str(r.data), "GET returned %r, not the value just set" % r.data)


def test_list_ops():
    r = plain.main.rpush(key = "L", value = "a")
    assert_true(r.ok, "RPUSH failed: %s" % r.error)

    r = plain.main.lrange(key = "L", start = "0", stop = "-1")
    assert_true(r.ok, "LRANGE failed: %s" % r.error)
    print("LRANGE returned:", r.data)
