# Faultbox recipes: Memcached
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/memcached.star", "memcached")
#
#     cache_down = fault_assumption("cache_down",
#         target = cache.main,
#         rules  = [memcached.server_error(command = "get")],
#     )
#
# Scope: Memcached text-protocol error lines that surface to clients as
# exceptions or retry triggers. The Memcached plugin matches rules by
# `command` (get, set, delete, incr, decr, add, replace, append,
# prepend, cas, stats, flush_all) and optionally `key` pattern. Error
# messages preserve the canonical "SERVER_ERROR ..." / "CLIENT_ERROR ..."
# prefixes so client-side error-type detection works verbatim.

memcached = struct(
    # server_error — "SERVER_ERROR out of memory storing object".
    # Classic Memcached error when the server can't evict fast enough
    # (memory pressure + large items). Drivers treat as retryable but
    # the canonical failure mode on under-provisioned caches.
    server_error = lambda command = "*", key = "*": error(
        command = command,
        key = key,
        message = "SERVER_ERROR out of memory storing object",
    ),

    # client_error — "CLIENT_ERROR bad command line format". Useful
    # when you want the driver to treat the response as a protocol
    # error (non-retryable) rather than a transient server issue.
    client_error = lambda command = "*", key = "*": error(
        command = command,
        key = key,
        message = "CLIENT_ERROR bad command line format",
    ),

    # not_stored — "NOT_STORED". Returned when `add` finds an
    # existing key or `replace` finds no key. Surfaces logic bugs in
    # cache-fill code that assume every set succeeds.
    not_stored = lambda command = "add", key = "*": error(
        command = command,
        key = key,
        message = "NOT_STORED",
    ),

    # exists — "EXISTS". Returned from `cas` when the CAS token is
    # stale (someone else wrote in the meantime). Tests optimistic
    # concurrency / retry loops in cache-backed counters.
    exists = lambda command = "cas", key = "*": error(
        command = command,
        key = key,
        message = "EXISTS",
    ),

    # item_too_large — "SERVER_ERROR object too large for cache".
    # Server's `-I` / item_size_max (default 1 MiB) was exceeded.
    # Forces app-side size checks + DLQ paths.
    item_too_large = lambda command = "set", key = "*": error(
        command = command,
        key = key,
        message = "SERVER_ERROR object too large for cache",
    ),

    # busy — "SERVER_ERROR busy". Server is under load (typically
    # during a slab reassignment or LRU maintenance). Tests client
    # back-off + fallback-to-origin patterns.
    busy = lambda command = "*", key = "*": error(
        command = command,
        key = key,
        message = "SERVER_ERROR busy",
    ),

    # slow_command — delays every command. Tests client read-timeout
    # and the "cache is slower than the DB" failure mode.
    slow_command = lambda duration = "3s", command = "*", key = "*": delay(
        command = command,
        key = key,
        delay = duration,
    ),

    # connection_drop — closes the TCP connection mid-command. Forces
    # pool to evict and reopen, triggers reconnect backoff paths.
    connection_drop = lambda command = "*", key = "*": drop(
        command = command,
        key = key,
    ),
)
