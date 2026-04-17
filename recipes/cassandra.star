# Faultbox recipes: Cassandra
#
# Curated failures for Cassandra services. Each recipe wraps the core
# proxy fault primitives with CQL pattern matching and canonical error
# messages that match what real Cassandra nodes emit.
#
# Usage:
#     load("./recipes/cassandra.star", "write_timeout", "unavailable")
#
#     broken = fault_assumption("broken",
#         target = cass.main,
#         rules  = [write_timeout(query = "INSERT*")],
#     )

# write_timeout simulates a coordinator-level write timeout. Drivers
# typically retry per their write-timeout retry policy.
def write_timeout(query = "INSERT*"):
    return error(
        query = query,
        message = "Operation timed out - received only 0 responses",
    )

# read_timeout simulates a read timeout. Triggers driver read-timeout
# retries.
def read_timeout(query = "SELECT*"):
    return error(
        query = query,
        message = "Operation timed out - received only 1 responses",
    )

# unavailable simulates insufficient replicas to satisfy consistency.
# This is the error code Cassandra drivers map to UnavailableException.
def unavailable(query = "*"):
    return error(
        query = query,
        message = "Cannot achieve consistency level QUORUM",
    )

# overloaded simulates a node under high load returning OverloadedException.
# Drivers typically retry on a different coordinator.
def overloaded(query = "*"):
    return error(
        query = query,
        message = "Node is overloaded",
    )

# slow_reads delays SELECT queries. Use to test client read-timeout
# handling and speculative execution.
def slow_reads(duration = "3s"):
    return delay(query = "SELECT*", delay = duration)

# slow_writes delays INSERT/UPDATE/DELETE statements.
def slow_writes(duration = "3s"):
    return delay(query = "INSERT*", delay = duration)

# connection_drop closes the connection mid-statement. Drivers surface
# this as a connection-lost exception and reconnect.
def connection_drop(query = "*"):
    return drop(query = query)

# schema_mismatch simulates a node reporting a different schema version.
# Drivers typically refresh their schema cache on this error.
def schema_mismatch(query = "*"):
    return error(
        query = query,
        message = "Schema version mismatch detected",
    )
