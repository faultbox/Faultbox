# Faultbox recipes: Cassandra
#
# Per RFC-018: one namespace struct per recipe file.
#
# Usage:
#     load("./recipes/cassandra.star", "cassandra")
#
#     broken = fault_assumption("quorum_lost",
#         target = cass.main,
#         rules  = [cassandra.unavailable()],
#     )

cassandra = struct(
    # write_timeout — coordinator-level write timeout.
    write_timeout = lambda query = "INSERT*": error(
        query = query,
        message = "Operation timed out - received only 0 responses",
    ),

    # read_timeout — read timeout. Triggers driver read-timeout retries.
    read_timeout = lambda query = "SELECT*": error(
        query = query,
        message = "Operation timed out - received only 1 responses",
    ),

    # unavailable — insufficient replicas to satisfy consistency.
    unavailable = lambda query = "*": error(
        query = query,
        message = "Cannot achieve consistency level QUORUM",
    ),

    # overloaded — node under high load (OverloadedException).
    overloaded = lambda query = "*": error(
        query = query,
        message = "Node is overloaded",
    ),

    # slow_reads delays SELECT queries. Tests client read-timeout and
    # speculative execution.
    slow_reads = lambda duration = "3s": delay(
        query = "SELECT*",
        delay = duration,
    ),

    # slow_writes delays INSERT/UPDATE/DELETE.
    slow_writes = lambda duration = "3s": delay(
        query = "INSERT*",
        delay = duration,
    ),

    # connection_drop closes the connection mid-statement.
    connection_drop = lambda query = "*": drop(query = query),

    # schema_mismatch — stale schema version. Drivers refresh their cache.
    schema_mismatch = lambda query = "*": error(
        query = query,
        message = "Schema version mismatch detected",
    ),
)
