# Faultbox recipes: NATS
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/nats.star", "nats")
#
#     slow_downstream = fault_assumption("slow_downstream",
#         target = bus.main,
#         rules  = [nats.slow_consumer(subject = "orders.>")],
#     )
#
# Scope: NATS server error conditions that surface to clients as
# protocol-level -ERR lines or connection-level failures. The NATS
# plugin matches rules by subject; wildcards (`*`, `>`) are honored as
# glob prefixes. Error messages embed the canonical NATS text so
# client-side error handlers (nats.ErrXxx sentinels, error-type checks)
# recognize them.

nats = struct(
    # slow_consumer — "Slow Consumer Detected". Server drops messages
    # destined for a subscriber that can't keep up with the publish
    # rate. Exposes back-pressure bugs in consumer loops and surfaces
    # lost-message paths that most apps never exercise.
    slow_consumer = lambda subject = ">": error(
        topic = subject,
        message = "Slow Consumer Detected - messages dropped",
    ),

    # no_responders — "503 No Responders". Request-reply request sent
    # to a subject with zero subscribers. Drivers see nats.ErrNoResponders;
    # tests the "what if the service we're calling isn't there" path.
    no_responders = lambda subject = ">": error(
        topic = subject,
        message = "503 No Responders available for request",
    ),

    # max_payload — "Maximum Payload Exceeded". Publish bigger than
    # the server's max_payload setting (default 1 MiB). Drivers
    # surface nats.ErrMaxPayload; app-side fallback + chunking tests.
    max_payload = lambda subject = ">": error(
        topic = subject,
        message = "Maximum Payload Exceeded",
    ),

    # authorization_violation — "Authorization Violation". Publisher
    # or subscriber lacks permission for the subject. Tests
    # credential-refresh + reconnect-with-new-creds flows.
    authorization_violation = lambda subject = ">": error(
        topic = subject,
        message = "Authorization Violation",
    ),

    # permissions_violation — "Permissions Violation for Publish to".
    # Subtly different from authorization_violation: user is
    # authenticated but the specific subject/operation is denied.
    permissions_violation = lambda subject = ">": error(
        topic = subject,
        message = "Permissions Violation for Publish to subject",
    ),

    # stale_connection — "Stale Connection". Server detected the
    # client connection is unresponsive and closing it. Drivers
    # reconnect through resolver + server-list failover paths.
    stale_connection = lambda subject = ">": error(
        topic = subject,
        message = "Stale Connection",
    ),

    # slow_delivery — delays message delivery. Tests consumer
    # processing deadlines and inflight-message assumptions.
    slow_delivery = lambda duration = "3s", subject = ">": delay(
        topic = subject,
        delay = duration,
    ),

    # connection_drop — closes the TCP connection mid-stream. Forces
    # driver reconnect through its server list + backoff policy.
    connection_drop = lambda subject = ">": drop(topic = subject),
)
