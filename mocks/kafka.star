# @faultbox/mocks/kafka.star
#
# Thin Starlark wrapper that builds a Kafka mock via the generic
# mock_service() primitive. The wrapper encodes Kafka-specific knobs
# (topics, partitions) into the opaque config= map that the Go kafka
# protocol plugin interprets.
#
# Usage:
#
#     load("@faultbox/mocks/kafka.star", "kafka")
#
#     bus = kafka.broker(
#         name      = "bus",
#         interface = interface("main", "kafka", 9092),
#         topics    = {"orders": [], "payments": []},
#     )
#
# Under the hood this calls mock_service() with config={"topics": ...,
# "partitions": ...}. The kafka plugin's ServeMock runs an in-process
# kfake broker (github.com/twmb/franz-go/pkg/kfake) that real
# kafka-go / franz-go / sarama clients speak to without modification.
#
# v0.8 scope: topic seeding (names + partition count). Pre-populating
# topics with messages is not yet supported — kfake has no public API
# for it. For now, produce seed messages from the test itself.

def _broker(name, interface, topics = {}, partitions = 1, depends_on = []):
    return mock_service(
        name,
        interface,
        config = {
            "topics":     topics,
            "partitions": partitions,
        },
        depends_on = depends_on,
    )

kafka = struct(
    broker = _broker,
)
