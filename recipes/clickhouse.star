# Faultbox recipes: ClickHouse
#
# Curated failures for ClickHouse services over the HTTP interface.
# Each recipe wraps the core primitives with ClickHouse-shaped exception
# messages that drivers parse out of the response body.
#
# Usage:
#     load("./recipes/clickhouse.star", "too_many_parts", "memory_limit")
#
#     overloaded = fault_assumption("overloaded",
#         target = ch.main,
#         rules  = [too_many_parts(query = "INSERT*")],
#     )

# too_many_parts is the canonical "insert rate exceeds merge rate" error.
# Drivers typically back off and retry; clients should slow their insert
# rate.
def too_many_parts(query = "INSERT*"):
    return error(
        query = query,
        message = "Too many parts (300). Merges are processing significantly slower than inserts",
    )

# memory_limit simulates queries that exceed the configured memory quota.
# Drivers raise a DB::Exception code 241.
def memory_limit(query = "SELECT*"):
    return error(
        query = query,
        message = "Memory limit (for query) exceeded",
    )

# table_not_exists simulates a reference to a missing table. Drivers
# typically surface this as code 60.
def table_not_exists(query = "*"):
    return error(
        query = query,
        message = "Table doesn't exist",
    )

# readonly_mode simulates the server refusing writes because it's in
# readonly mode (common during maintenance).
def readonly_mode(query = "INSERT*"):
    return error(
        query = query,
        message = "Cannot execute query in readonly mode",
    )

# slow_analytics delays SELECT queries. Use to test dashboard and ETL
# timeout handling.
def slow_analytics(duration = "5s"):
    return delay(query = "SELECT*", delay = duration)

# slow_ingest delays INSERT statements. Tests producer back-pressure.
def slow_ingest(duration = "3s"):
    return delay(query = "INSERT*", delay = duration)

# connection_drop closes the HTTP connection mid-query. Drivers
# reconnect per their pool settings.
def connection_drop(query = "*"):
    return drop(query = query)

# replica_stale returns an error indicating the replica can't be used
# because it's too far behind the leader.
def replica_stale(query = "SELECT*"):
    return error(
        query = query,
        message = "Replica is too far behind",
    )
