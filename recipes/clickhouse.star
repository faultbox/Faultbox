# Faultbox recipes: ClickHouse
#
# Per RFC-018: one namespace struct per recipe file.
#
# Usage:
#     load("./recipes/clickhouse.star", "clickhouse")
#
#     overloaded = fault_assumption("overloaded",
#         target = ch.main,
#         rules  = [clickhouse.too_many_parts(query = "INSERT*")],
#     )

clickhouse = struct(
    # too_many_parts — insert rate exceeds merge rate. Drivers back off.
    too_many_parts = lambda query = "INSERT*": error(
        query = query,
        message = "Too many parts (300). Merges are processing significantly slower than inserts",
    ),

    # memory_limit — query exceeds configured memory quota (code 241).
    memory_limit = lambda query = "SELECT*": error(
        query = query,
        message = "Memory limit (for query) exceeded",
    ),

    # table_not_exists — missing table (code 60).
    table_not_exists = lambda query = "*": error(
        query = query,
        message = "Table doesn't exist",
    ),

    # readonly_mode — server refuses writes (maintenance).
    readonly_mode = lambda query = "INSERT*": error(
        query = query,
        message = "Cannot execute query in readonly mode",
    ),

    # slow_analytics delays SELECT queries. Dashboard / ETL timeout tests.
    slow_analytics = lambda duration = "5s": delay(
        query = "SELECT*",
        delay = duration,
    ),

    # slow_ingest delays INSERT statements. Producer back-pressure tests.
    slow_ingest = lambda duration = "3s": delay(
        query = "INSERT*",
        delay = duration,
    ),

    # connection_drop closes the HTTP connection mid-query.
    connection_drop = lambda query = "*": drop(query = query),

    # replica_stale — replica too far behind leader for consistent read.
    replica_stale = lambda query = "SELECT*": error(
        query = query,
        message = "Replica is too far behind",
    ),
)
