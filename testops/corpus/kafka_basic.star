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

def test_publish_to_orders_topic():
    # publish() returns successfully when the broker accepts the message.
    # The assertion is the absence of a panic / error — kafka mock step
    # helpers raise on broker rejection.
    bus.main.publish(topic = "orders", key = "o-1", value = '{"id":1}')

def test_publish_to_payments_topic():
    bus.main.publish(topic = "payments", key = "p-1", value = '{"amount":100}')
