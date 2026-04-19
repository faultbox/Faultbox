# Faultbox recipes: AMQP 0-9-1 (RabbitMQ)
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/amqp.star", "amqp")
#
#     queue_down = fault_assumption("queue_down",
#         target = mq.main,
#         rules  = [amqp.channel_error(routing_key = "orders.*")],
#     )
#
# Scope: AMQP 0-9-1 error conditions most commonly seen in production
# RabbitMQ deployments. The AMQP plugin matches rules by routing key
# (`topic=` kwarg); wildcards (`*`, `#`) are honored as glob prefixes.
# Error messages use RabbitMQ's canonical text so `amqp.Error.Code` +
# reason checks on the client side behave like real production failures.

amqp = struct(
    # channel_error — AMQP 0-9-1 channel exception, soft error. The
    # channel is closed; the connection stays alive. Apps must
    # recreate the channel before publishing or consuming again.
    channel_error = lambda routing_key = "#": error(
        topic = routing_key,
        message = "CHANNEL_ERROR - precondition failed",
    ),

    # connection_error — AMQP connection exception, hard error.
    # Entire connection is torn down; clients reconnect + redeclare
    # all channels/consumers/bindings. Tests reconnect + resubscribe
    # logic.
    connection_error = lambda routing_key = "#": error(
        topic = routing_key,
        message = "CONNECTION_FORCED - broker forced connection close",
    ),

    # resource_locked — "RESOURCE_LOCKED - cannot obtain exclusive
    # access to locked queue". Typically an exclusive consumer or
    # single-active-consumer policy blocked this client. Surfaces
    # failover + leadership-election bugs.
    resource_locked = lambda routing_key = "#": error(
        topic = routing_key,
        message = "RESOURCE_LOCKED - cannot obtain exclusive access to locked queue",
    ),

    # access_refused — "ACCESS_REFUSED - operation not permitted on
    # the default exchange" or similar. Tests credential-refresh +
    # vhost-permission-change flows.
    access_refused = lambda routing_key = "#": error(
        topic = routing_key,
        message = "ACCESS_REFUSED - operation not permitted",
    ),

    # precondition_failed — "PRECONDITION_FAILED - inequivalent arg
    # 'durable' for queue". Raised when redeclaring a queue with
    # different parameters than the existing one. Common after config
    # changes; exposes environments that expect re-declare to succeed.
    precondition_failed = lambda routing_key = "#": error(
        topic = routing_key,
        message = "PRECONDITION_FAILED - inequivalent queue arguments",
    ),

    # publish_nack — broker refused a publisher-confirmed publish.
    # Used when publisher confirms are enabled and the message cannot
    # be routed or queued (e.g. queue full with reject-publish policy).
    # Forces the client's nack-handler code path.
    publish_nack = lambda routing_key = "#": error(
        topic = routing_key,
        message = "publish nacked by broker - queue unavailable",
    ),

    # broker_unavailable — simulates the broker being unreachable or
    # refusing new connections (common during rolling restarts of a
    # RabbitMQ cluster). Clients should failover to another node.
    broker_unavailable = lambda routing_key = "#": error(
        topic = routing_key,
        message = "broker unavailable - connection refused",
    ),

    # slow_publish — delays every publish. Tests publisher confirm
    # deadlines + back-pressure in message-producing code paths.
    slow_publish = lambda duration = "3s", routing_key = "#": delay(
        topic = routing_key,
        delay = duration,
    ),

    # connection_drop — closes the TCP connection mid-frame. Forces
    # the AMQP client to reconnect and redeclare topology; exposes
    # any state the client assumed the broker remembers.
    connection_drop = lambda routing_key = "#": drop(topic = routing_key),
)
