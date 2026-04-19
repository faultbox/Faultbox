# Faultbox recipes: HTTP/1.x
#
# Per RFC-018: one namespace struct per recipe file — `http.rate_limited`
# never collides with `http2.rate_limited` when both are loaded.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/http.star", "http")
#
#     rate_limited = fault_assumption("api_rate_limited",
#         target = api.main,
#         rules  = [http.rate_limited(path = "/api/**")],
#     )
#
# Scope: HTTP/1.x status codes + connection-level behavior. Semantically
# mirrors @faultbox/recipes/http2.star for apps that speak plain HTTP/1.x,
# but replaces `stream_reset` (HTTP/2-specific) with `connection_drop`
# (HTTP/1.x closes the TCP connection instead).

http = struct(
    # rate_limited — 429 Too Many Requests with a Retry-After hint.
    # Triggers client back-off and retry-with-jitter paths.
    rate_limited = lambda path = "/*": response(
        path = path,
        status = 429,
        body = "{\"error\":\"rate limited\",\"retry_after\":\"1s\"}",
    ),

    # server_error — 500 Internal Server Error. Generic
    # "something went wrong" response that every HTTP client must handle.
    server_error = lambda path = "/*": error(
        path = path,
        status = 500,
        message = "internal server error",
    ),

    # service_unavailable — 503 Service Unavailable. Retryable per
    # HTTP semantics; tests client retry + circuit-breaker behavior.
    service_unavailable = lambda path = "/*": error(
        path = path,
        status = 503,
        message = "service unavailable",
    ),

    # gateway_timeout — 504 Gateway Timeout. Upstream origin took too
    # long for the intermediary. Exposes the edge/middle retry
    # decisions in proxy + LB setups.
    gateway_timeout = lambda path = "/*": error(
        path = path,
        status = 504,
        message = "gateway timeout",
    ),

    # slow_endpoint — delays the response by `duration`. Tests client
    # read-timeout and deadline-propagation behavior.
    slow_endpoint = lambda path = "/*", duration = "3s": delay(
        path = path,
        delay = duration,
    ),

    # maintenance_window — 503 with a long Retry-After body, matching
    # how most LBs return "we're deploying" during rolling restarts.
    maintenance_window = lambda path = "/*": response(
        path = path,
        status = 503,
        body = "{\"error\":\"maintenance\",\"retry_after\":\"60s\"}",
    ),

    # connection_drop — closes the TCP connection mid-request. HTTP/1.x
    # analog of http2.stream_reset. Forces the client's keep-alive pool
    # to evict and reopen.
    connection_drop = lambda path = "/*": drop(path = path),

    # flaky — returns 500 a fraction of the time. Retry-policy and
    # exponential-backoff tests.
    flaky = lambda path = "/*", probability = "20%": error(
        path = path,
        status = 500,
        message = "flaky server",
        probability = probability,
    ),

    # unauthorized — 401 Unauthorized. Token-expiry and refresh-flow
    # tests; exposes auth handlers that swallow the status silently.
    unauthorized = lambda path = "/*": error(
        path = path,
        status = 401,
        message = "unauthorized",
    ),

    # forbidden — 403 Forbidden. Distinct from 401: caller is known
    # but lacks permission. Tests authorization-failure paths.
    forbidden = lambda path = "/*": error(
        path = path,
        status = 403,
        message = "forbidden",
    ),
)
