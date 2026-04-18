# Faultbox recipes: MongoDB
#
# Per RFC-018: namespace struct (prevents name collisions across protocols).
# Per RFC-019: stdlib ships embedded in the faultbox binary, loaded via
# the @faultbox/ prefix — no local recipes/ directory needed.
#
# Usage:
#     load("@faultbox/recipes/mongodb.star", "mongodb")
#
#     broken = fault_assumption("broken",
#         target = db.main,
#         rules  = [mongodb.disk_full(collection = "orders")],
#     )

mongodb = struct(
    # disk_full simulates a full data disk on insert — drivers surface
    # this as a WriteException and clients trigger back-pressure.
    disk_full = lambda collection = "*": error(
        collection = collection,
        op = "insert",
        message = "assertion: 10334 disk full",
    ),

    # auth_failed rejects SASL handshakes. Verifies the SUT surfaces
    # credential errors rather than hanging.
    auth_failed = lambda: error(
        op = "saslStart",
        message = "Authentication failed",
    ),

    # replica_unavailable simulates a write concern failure — replica
    # set election in progress, no primary available.
    replica_unavailable = lambda collection = "*": error(
        collection = collection,
        op = "*",
        message = "not primary; no replica set members available",
    ),

    # slow_query delays find() operations. Tests client-side query
    # timeouts and retry behavior.
    slow_query = lambda collection = "*", duration = "3s": delay(
        collection = collection,
        op = "find",
        delay = duration,
    ),

    # slow_writes delays insert operations. Tests write-timeout handling
    # and transaction rollback.
    slow_writes = lambda collection = "*", duration = "3s": delay(
        collection = collection,
        op = "insert",
        delay = duration,
    ),

    # connection_drop closes connections mid-command. Triggers the
    # driver's reconnect + retry path.
    connection_drop = lambda collection = "*", op = "*": drop(
        collection = collection,
        op = op,
    ),

    # duplicate_key_error simulates a unique index violation on insert.
    # Common failure during idempotency-key collisions.
    duplicate_key_error = lambda collection = "*": error(
        collection = collection,
        op = "insert",
        message = "E11000 duplicate key error",
    ),

    # write_conflict simulates a TransientTransactionError mid-transaction.
    # Drivers retry per the MongoDB transaction retry protocol.
    write_conflict = lambda collection = "*": error(
        collection = collection,
        op = "update",
        message = "WriteConflict: TransientTransactionError",
    ),
)
