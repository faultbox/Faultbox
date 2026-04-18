# Faultbox recipes: HTTP/2
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib is embedded in the faultbox binary (@faultbox/ prefix).
#
# Usage:
#     load("@faultbox/recipes/http2.star", "http2")
#
#     faulty = fault_assumption("faulty_api",
#         target = api.main,
#         rules  = [http2.rate_limited(path = "/api/**")],
#     )

http2 = struct(
    # rate_limited returns 429 with a Retry-After hint.
    rate_limited = lambda path = "/*": response(
        path = path,
        status = 429,
        body = "{\"error\":\"rate limited\",\"retry_after\":\"1s\"}",
    ),

    # server_error returns 500 — the generic "something went wrong".
    server_error = lambda path = "/*": error(
        path = path,
        status = 500,
        message = "internal server error",
    ),

    # service_unavailable returns 503 (retryable per HTTP semantics).
    service_unavailable = lambda path = "/*": error(
        path = path,
        status = 503,
        message = "service unavailable",
    ),

    # gateway_timeout returns 504.
    gateway_timeout = lambda path = "/*": error(
        path = path,
        status = 504,
        message = "gateway timeout",
    ),

    # slow_endpoint delays matching requests.
    slow_endpoint = lambda path = "/*", duration = "3s": delay(
        path = path,
        delay = duration,
    ),

    # maintenance_window returns 503 with Retry-After.
    maintenance_window = lambda path = "/*": response(
        path = path,
        status = 503,
        body = "{\"error\":\"maintenance\",\"retry_after\":\"60s\"}",
    ),

    # stream_reset closes the HTTP/2 stream mid-request.
    stream_reset = lambda path = "/*": drop(path = path),

    # flaky returns 500 a fraction of the time. Retry tests.
    flaky = lambda path = "/*", probability = "20%": error(
        path = path,
        status = 500,
        message = "flaky endpoint",
        probability = probability,
    ),

    # unauthorized returns 401. Auth-error / token-refresh tests.
    unauthorized = lambda path = "/*": error(
        path = path,
        status = 401,
        message = "unauthorized",
    ),

    # forbidden returns 403. Authorization (not authentication) failures.
    forbidden = lambda path = "/*": error(
        path = path,
        status = 403,
        message = "forbidden",
    ),
)
