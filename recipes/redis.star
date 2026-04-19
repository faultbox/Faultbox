# Faultbox recipes: Redis
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/redis.star", "redis")
#
#     cache_oom = fault_assumption("cache_oom_fallback",
#         target = cache.main,
#         rules  = [redis.oom()],
#     )
#
# Scope: RESP error strings Redis emits under operational failure. The
# Redis plugin matches rules by key pattern (or wildcards); the message
# is what go-redis / redigo / raw RESP clients see verbatim as an
# "ERR ..." reply. All recipes prefix with the canonical Redis error
# tag so client-side error-type detection ("strings.HasPrefix(err, ...)"
# or error-type checks) works.

redis = struct(
    # oom — "OOM command not allowed when used memory > 'maxmemory'".
    # Writes rejected once maxmemory is reached; reads still succeed.
    # Silent-skip bugs in cache-cleaning logic live in this error path.
    oom = lambda key = "*": error(
        key = key,
        message = "OOM command not allowed when used memory > 'maxmemory'",
    ),

    # cluster_down — "CLUSTERDOWN The cluster is down". Cluster lost
    # quorum; most commands fail until a majority of masters are back.
    cluster_down = lambda key = "*": error(
        key = key,
        message = "CLUSTERDOWN The cluster is down",
    ),

    # loading — "LOADING Redis is loading the dataset in memory". Server
    # just restarted and is replaying its RDB/AOF; every command fails
    # until loading completes. Apps without retry see reads-always-fail.
    loading = lambda key = "*": error(
        key = key,
        message = "LOADING Redis is loading the dataset in memory",
    ),

    # readonly_replica — "READONLY You can't write against a read only
    # replica." Writes accidentally routed to a replica after failover
    # fail silently in some client libraries, leaving stale state.
    readonly_replica = lambda key = "*": error(
        key = key,
        message = "READONLY You can't write against a read only replica.",
    ),

    # busy — "BUSY Redis is busy running a script. You can only call
    # SCRIPT KILL or SHUTDOWN NOSAVE." A Lua script monopolized the
    # single-threaded server; all other commands block until it ends.
    busy = lambda key = "*": error(
        key = key,
        message = "BUSY Redis is busy running a script. You can only call SCRIPT KILL or SHUTDOWN NOSAVE.",
    ),

    # noauth — "NOAUTH Authentication required". Server restarted into
    # an authenticated mode the client didn't handshake with; all
    # commands fail until the client re-AUTHs.
    noauth = lambda key = "*": error(
        key = key,
        message = "NOAUTH Authentication required.",
    ),

    # wrongtype — "WRONGTYPE Operation against a key holding the wrong
    # kind of value". Tests client-side type-mismatch handling when a
    # key's semantics drift (e.g., migration reshaped a string to hash).
    wrongtype = lambda key = "*": error(
        key = key,
        message = "WRONGTYPE Operation against a key holding the wrong kind of value",
    ),

    # slow_command — delays every command. Tests pool timeout and
    # client-side deadline propagation.
    slow_command = lambda duration = "3s", key = "*": delay(
        key = key,
        delay = duration,
    ),

    # connection_drop closes the client connection mid-command. Forces
    # pool to evict and reconnect.
    connection_drop = lambda key = "*": drop(key = key),
)
