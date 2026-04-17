# Faultbox recipes: MongoDB
#
# Curated common failures for MongoDB services. Each recipe is a thin wrapper
# over the core proxy fault primitives (error/delay/drop) that encodes
# canonical MongoDB server error messages and codes.
#
# Usage:
#     load("./recipes/mongodb.star", "disk_full", "replica_unavailable")
#
#     mongo_broken = fault_assumption("mongo_broken",
#         target = db.main,
#         rules  = [disk_full(collection = "orders")],
#     )
#
# These are building blocks. Compose or override as needed — nothing in
# Faultbox forces you to use them. Prefer recipes for failures that match
# real MongoDB operational incidents; drop to `error(...)` directly for
# one-off synthetic faults.

# disk_full simulates a full data disk on insert — drivers surface this as
# a WriteException and clients typically trigger back-pressure.
def disk_full(collection = "*"):
    return error(
        collection = collection,
        op = "insert",
        message = "assertion: 10334 disk full",
    )

# auth_failed rejects authentication handshakes. Useful to verify the
# system under test surfaces credential errors rather than hanging.
def auth_failed():
    return error(
        op = "saslStart",
        message = "Authentication failed",
    )

# replica_unavailable simulates a write concern failure because quorum
# cannot be reached. This is the shape drivers see during replica set
# elections.
def replica_unavailable(collection = "*"):
    return error(
        collection = collection,
        op = "*",
        message = "not primary; no replica set members available",
    )

# slow_query delays every find() by the given duration. Use to test
# client-side query timeouts and retry behavior.
def slow_query(collection = "*", duration = "3s"):
    return delay(
        collection = collection,
        op = "find",
        delay = duration,
    )

# slow_writes delays every insert/update/delete. Tests write timeout
# handling and transaction rollback behavior.
def slow_writes(collection = "*", duration = "3s"):
    return delay(
        collection = collection,
        op = "insert",
        delay = duration,
    )

# connection_drop closes connections mid-command on the given collection
# and op. Triggers the driver's reconnect+retry path.
def connection_drop(collection = "*", op = "*"):
    return drop(
        collection = collection,
        op = op,
    )

# duplicate_key_error simulates a unique index violation on insert.
# Common real failure during idempotency-key collisions.
def duplicate_key_error(collection = "*"):
    return error(
        collection = collection,
        op = "insert",
        message = "E11000 duplicate key error",
    )

# write_conflict simulates a TransientTransactionError mid-transaction.
# Drivers should retry per the MongoDB transaction retry protocol.
def write_conflict(collection = "*"):
    return error(
        collection = collection,
        op = "update",
        message = "WriteConflict: TransientTransactionError",
    )
