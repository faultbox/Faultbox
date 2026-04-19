# Faultbox recipes: Postgres
#
# Per RFC-018: one namespace struct per recipe file — postgres.deadlock()
# never collides with mysql.deadlock() when both recipes are loaded.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/postgres.star", "postgres")
#
#     deadlock_retry = fault_assumption("deadlock_retry",
#         target = db.main,
#         rules  = [postgres.deadlock()],
#     )
#
# Scope: Postgres SQLSTATE error classes. The proxy's Postgres plugin
# matches rules by SQL query pattern; the error message below is what
# pq / pgx surface up to application code. SQLSTATE codes embedded in
# messages so driver error-type checks (errors.Is, pgconn.PgError.Code)
# recognize them.

postgres = struct(
    # deadlock — SQLSTATE 40P01 (deadlock_detected). Two transactions
    # circular-wait on row locks; Postgres picks a victim. Drivers
    # surface pgerrcode.DeadlockDetected; apps with retry loop, apps
    # without retry crash or leak the transaction.
    deadlock = lambda query = "*": error(
        query = query,
        message = "ERROR: deadlock detected (SQLSTATE 40P01)",
    ),

    # lock_not_available — SQLSTATE 55P03 (lock_not_available). Query
    # waited past lock_timeout or statement_timeout. Driver surfaces
    # the message verbatim; connection stays alive.
    lock_not_available = lambda query = "*": error(
        query = query,
        message = "ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)",
    ),

    # serialization_failure — SQLSTATE 40001. Serializable / repeatable
    # read transaction aborted because concurrent commit invalidated
    # its snapshot. Drivers surface pgerrcode.SerializationFailure;
    # retry loops built on 40P01 only miss this path.
    serialization_failure = lambda query = "*": error(
        query = query,
        message = "ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)",
    ),

    # too_many_connections — SQLSTATE 53300. Server rejects new
    # connections because max_connections is saturated. Exposes
    # nil-pointer bugs in connection-pool init paths that don't
    # surface the error.
    too_many_connections = lambda: error(
        query = "*",
        message = "FATAL: sorry, too many clients already (SQLSTATE 53300)",
    ),

    # read_only_transaction — SQLSTATE 25006. Writes accidentally routed
    # to a hot-standby / read-replica fail here; reads succeed. Default
    # targets INSERT; override query= for UPDATE/DELETE.
    read_only_transaction = lambda query = "INSERT*": error(
        query = query,
        message = "ERROR: cannot execute INSERT in a read-only transaction (SQLSTATE 25006)",
    ),

    # disk_full — SQLSTATE 53100 (disk_full). INSERTs against a full
    # tablespace reject. Drivers surface pgerrcode.DiskFull; app-side
    # retries do not help.
    disk_full = lambda query = "INSERT*": error(
        query = query,
        message = "ERROR: could not extend file: No space left on device (SQLSTATE 53100)",
    ),

    # admin_shutdown — SQLSTATE 57P01. Server is shutting down (graceful
    # restart, failover, planned maintenance). Drivers see the
    # connection terminate with this SQLSTATE; pools must evict the
    # connection rather than retry on it.
    admin_shutdown = lambda query = "*": error(
        query = query,
        message = "FATAL: terminating connection due to administrator command (SQLSTATE 57P01)",
    ),

    # connection_failure — SQLSTATE 08006. Simulated by dropping the
    # connection mid-query; drivers see connection_failure and must
    # reconnect through the pool.
    connection_failure = lambda query = "*": drop(query = query),

    # slow_query — delays every statement by duration. Tests client
    # statement_timeout handling and context-deadline propagation.
    slow_query = lambda duration = "3s", query = "*": delay(
        query = query,
        delay = duration,
    ),

    # slow_writes — delays write statements only. Useful for testing how
    # read-heavy paths degrade when the write tier is slow.
    slow_writes = lambda duration = "3s", query = "INSERT*": delay(
        query = query,
        delay = duration,
    ),
)
