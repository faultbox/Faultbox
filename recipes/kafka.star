# Faultbox recipes: Kafka
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/kafka.star", "kafka")
#
#     rebalance = fault_assumption("rebalance_during_consume",
#         target = bus.main,
#         rules  = [kafka.rebalancing()],
#     )
#
# Scope: Kafka broker error codes that surface to clients as exceptions.
# The Kafka plugin matches rules by topic name (+ eventually operation,
# see notes on kafka.not_leader_for_partition below). Error messages
# embed the canonical Kafka error code number so driver error handlers
# recognize them (UnknownTopicOrPartitionException, etc.).

kafka = struct(
    # not_leader_for_partition — error code 6 (NOT_LEADER_FOR_PARTITION).
    # Produce request hit a broker that is no longer the partition leader
    # — happens during leadership election or broker rolls. Drivers
    # refresh metadata and retry; infinite-retry bugs live here.
    not_leader_for_partition = lambda topic = "*": error(
        topic = topic,
        message = "NotLeaderForPartitionException (error code 6): This server is not the leader for that topic-partition",
    ),

    # rebalancing — simplified REBALANCE_IN_PROGRESS (code 27). Forces
    # consumers into their rebalance-handler path. Real rebalances are a
    # multi-step group-coordinator dance; this recipe triggers the same
    # driver-side code path without simulating the full state machine.
    rebalancing = lambda topic = "*": error(
        topic = topic,
        message = "RebalanceInProgressException (error code 27): The group is rebalancing",
    ),

    # offset_out_of_range — error code 1 (OFFSET_OUT_OF_RANGE). Consumer
    # asks for an offset that's past the log head (or before retention
    # cutoff). Drivers with auto.offset.reset=none die here; silent
    # consumer deaths hide in this error path.
    offset_out_of_range = lambda topic = "*": error(
        topic = topic,
        message = "OffsetOutOfRangeException (error code 1): The requested offset is not within the range of offsets maintained by the server",
    ),

    # message_too_large — error code 10 (MESSAGE_TOO_LARGE). Produce
    # rejected because payload exceeds message.max.bytes. No DLQ in most
    # apps = event lost forever.
    message_too_large = lambda topic = "*": error(
        topic = topic,
        message = "RecordTooLargeException (error code 10): The request included a message larger than the max message size the server will accept",
    ),

    # coordinator_not_available — error code 15 (COORDINATOR_NOT_AVAILABLE).
    # Consumer-group coordinator is down or in the middle of election.
    # Drivers retry FindCoordinator; shutdown paths that don't wait for
    # coordinator re-election hang on stop.
    coordinator_not_available = lambda topic = "*": error(
        topic = topic,
        message = "CoordinatorNotAvailableException (error code 15): The coordinator is not available",
    ),

    # broker_overloaded — simulates request throttling / load shedding.
    # Useful for testing client back-pressure.
    broker_overloaded = lambda topic = "*": error(
        topic = topic,
        message = "KafkaException: broker overloaded — request quota exceeded",
    ),

    # slow_produce — delays produce requests. Tests producer batching
    # and linger.ms behavior under latency.
    slow_produce = lambda duration = "3s", topic = "*": delay(
        topic = topic,
        delay = duration,
    ),

    # connection_drop closes the TCP connection mid-request. Forces
    # driver reconnect through its metadata + coordinator discovery path.
    connection_drop = lambda topic = "*": drop(topic = topic),
)
