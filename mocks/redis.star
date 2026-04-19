# @faultbox/mocks/redis.star
#
# Starlark wrapper around mock_service() for Redis mocks. Stands up an
# in-process Redis server (miniredis) so real go-redis / redigo clients
# connect, GET / SET / DEL / INCR / EXISTS / TTL, and observe seeded state.
#
# Usage:
#
#     load("@faultbox/mocks/redis.star", "redis")
#
#     cache = redis.server(
#         name      = "cache",
#         interface = interface("main", "redis", 6379),
#         state = {
#             "config:max_retries": "3",
#             "config:timeout_ms":  "5000",
#             "flag:new_ui":        "true",
#         },
#     )
#
# Scope: string cache + counters (80% of real Redis usage). Streams,
# pub/sub, Lua scripting inherited from miniredis; not all commands work
# identically to a real Redis, so specs that depend on exotic features
# should run the real server.

def _server(name, interface, state = {}, depends_on = []):
    return mock_service(
        name,
        interface,
        config = {"state": state},
        depends_on = depends_on,
    )

redis = struct(
    server = _server,
)
