# NATS: every step result asserted.
#
# No auth by default, so this is the "does the step work at all" half of the
# audit — the question that went unasked long enough for two credential bugs to
# ship in other protocols.

bus = service("nats",
    interface("main", "nats", 4222),
    image = "nats:2.10-alpine",
    healthcheck = ready(timeout = "60s"),
)


def test_publish():
    r = bus.main.publish(subject = "orders.created", data = "{\"id\":1}")
    assert_true(r.ok, "publish failed: %s" % r.error)


def test_publish_many():
    for i in range(5):
        r = bus.main.publish(subject = "orders.created", data = "msg-%d" % i)
        assert_true(r.ok, "publish %d failed: %s" % (i, r.error))
