# testops/corpus/kafka_basic.star — Kafka mock in isolation.
#
# Exercises the kafka.broker mock (in-process kfake) with seeded topic
# names and a publish step. Catches drift in the kfake wrapper and the
# Starlark kafka.broker recipe independently of mock_demo.
#
# Run directly:  faultbox test testops/corpus/kafka_basic.star

load("@faultbox/mocks/kafka.star", "kafka")

bus = kafka.broker(
    name      = "bus",
    interface = interface("main", "kafka", 19093),
    topics    = {"orders": [], "payments": []},
)

# Both tests used to discard the publish result, on the reasoning that "the
# assertion is the absence of a panic — kafka mock step helpers raise on broker
# rejection". That is not how a step reports failure: a publish the broker never
# accepted returns ok=False, it does not raise. So these tests asserted nothing
# and would have passed against a completely dead broker.
#
# Found by TEST_NO_ASSERTIONS (RFC-052) on its first run over this corpus.
# Assertions emit no events, so the golden trace is unchanged.

def test_publish_to_orders_topic():
    r = bus.main.publish(topic = "orders", key = "o-1", value = '{"id":1}')
    assert_true(r.ok, "publish to 'orders' failed: %s" % r.error)

def test_publish_to_payments_topic():
    r = bus.main.publish(topic = "payments", key = "p-1", value = '{"amount":100}')
    assert_true(r.ok, "publish to 'payments' failed: %s" % r.error)
