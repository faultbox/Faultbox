# faultbox.star — RFC-014 Kafka Test (apache/kafka:3.7.0, KRaft, no ZooKeeper)
#
# Purpose: verify that Unix socket SCM_RIGHTS fd passing (RFC-014) works
# with multi-process containers. apache/kafka uses a shell startup chain:
#   /__cacert_entrypoint.sh → /etc/kafka/docker/run → exec kafka-server-start.sh
# The shim sends the seccomp fd via Unix socket BEFORE exec'ing, so the fd
# is acquired regardless of the subsequent exec chain.
#
# Run in Lima VM:
#   faultbox test faultbox.star
#
# Expected: "seccomp listener acquired" in logs for kafka container,
# and test_kafka_rfc014 passes (write fault fires → publish fails).

kafka = service("kafka",
    interface("main", "kafka", 9092),
    image = "apache/kafka:3.7.0",
    env = {
        # KRaft mode (no ZooKeeper). CLUSTER_ID has a built-in default
        # in the image but we set it explicitly for reproducibility.
        "CLUSTER_ID": "5L6g3nShT-eMCtK--X86sw",
        "KAFKA_PROCESS_ROLES": "broker,controller",
        "KAFKA_NODE_ID": "1",
        "KAFKA_CONTROLLER_QUORUM_VOTERS": "1@localhost:9093",
        # Two listeners: broker (external) and controller (internal).
        "KAFKA_LISTENERS": "PLAINTEXT://:9092,CONTROLLER://:9093",
        "KAFKA_ADVERTISED_LISTENERS": "PLAINTEXT://localhost:9092",
        "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT",
        "KAFKA_CONTROLLER_LISTENER_NAMES": "CONTROLLER",
        "KAFKA_INTER_BROKER_LISTENER_NAME": "PLAINTEXT",
        "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR": "1",
        "KAFKA_AUTO_CREATE_TOPICS_ENABLE": "true",
        "KAFKA_LOG_RETENTION_HOURS": "1",
    },
    ports = {9092: 9092},
    # kafka_ready() uses DialLeader (not just TCP) to ensure the broker can
    # handle produce requests, not just accept connections via docker-proxy.
    healthcheck = kafka_ready("localhost:9092", timeout="120s"),
)

# --- RFC-014 Verification Test ---

def test_kafka_rfc014():
    """RFC-014 end-to-end: seccomp fd acquired for JVM, fault injection confirmed.

    This test runs in a single Kafka session to avoid port-cycling timing issues.

    Phase 1 (happy path): publish succeeds — verifies the container started correctly
    and that the Kafka client can reach the broker. If the healthcheck is lying
    (e.g., docker-proxy accepted but Kafka isn't ready), this publish will fail first.

    Phase 2 (fault path): with write=deny active, publish must fail. This only
    passes if the seccomp fd was actually acquired for the Kafka JVM
    (via RFC-014 Unix socket SCM_RIGHTS). If the shim fell back to no-seccomp,
    the fault would never fire and publish would succeed — causing the assert to fail.

    The seccomp filter intercepts JVM writes across ALL threads (confirmed by
    hundreds of writev:allow events in --debug mode during startup).
    """
    # Phase 1: publish without fault.
    result = kafka.main.publish(topic="rfc014-topic", key="k1", data="hello-from-faultbox")
    assert_true(result.ok, "phase1 publish failed (kafka not ready?): " + result.error)

    # Phase 2: write=deny must fire on Kafka JVM.
    def fault_scenario():
        r = kafka.main.publish(topic="fault-topic", data="should-fail")
        assert_true(
            not r.ok,
            "write=deny did not fire — seccomp fd may not have been acquired (RFC-014 regression): " + r.error
        )
    fault(kafka, write=deny("EIO"), run=fault_scenario)
