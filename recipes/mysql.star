# Faultbox recipes: MySQL
#
# Per RFC-018: one namespace struct per recipe file — mysql.deadlock()
# never collides with postgres.deadlock() when both recipes are loaded.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/mysql.star", "mysql")
#
#     deadlock_retry = fault_assumption("deadlock_retry",
#         target = db.main,
#         rules  = [mysql.deadlock()],
#     )
#
# Scope: MySQL 5.7 / 8.0 error packets. The proxy's MySQL plugin matches
# rules by SQL query pattern; the error message below is what drivers
# surface up to application code.

mysql = struct(
    # deadlock — error 1213, Two transactions circular-wait on row locks.
    # Drivers surface ER_LOCK_DEADLOCK; apps with retry logic loop, apps
    # without retry crash or leak the transaction.
    deadlock = lambda query = "*": error(
        query = query,
        message = "Deadlock found when trying to get lock; try restarting transaction",
    ),

    # lock_wait_timeout — error 1205. Statement waited past
    # innodb_lock_wait_timeout (default 50s). Driver error surfaces the
    # exact message below; connection stays alive.
    lock_wait_timeout = lambda query = "*": error(
        query = query,
        message = "Lock wait timeout exceeded; try restarting transaction",
    ),

    # too_many_connections — error 1040. Server rejects new connections
    # because max_connections is saturated. Exposes nil-pointer bugs in
    # connection-pool init paths that don't surface the error.
    too_many_connections = lambda: error(
        query = "*",
        message = "Too many connections",
    ),

    # read_only_replica — error 1290, --read-only. Writes accidentally
    # routed to a replica fail here; reads succeed. Default targets
    # INSERT statements; override query= for UPDATE/DELETE.
    read_only_replica = lambda query = "INSERT*": error(
        query = query,
        message = "The MySQL server is running with the --read-only option so it cannot execute this statement",
    ),

    # disk_full — error 1114. INSERTs against a full tablespace reject.
    # Drivers surface ER_RECORD_FILE_FULL; app-side retries do not help.
    disk_full = lambda query = "INSERT*": error(
        query = query,
        message = "The table is full",
    ),

    # gone_away — client-side error 2006. Simulated by dropping the
    # connection mid-query; drivers see the classic
    # "MySQL server has gone away" and must reconnect.
    gone_away = lambda query = "*": drop(query = query),

    # slow_query — delays every statement by duration. Tests client
    # query-timeout handling and transaction deadline behavior.
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
