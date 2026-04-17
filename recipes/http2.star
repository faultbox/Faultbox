# Faultbox recipes: HTTP/2
#
# Curated common failures for HTTP/2 services. These wrap the core proxy
# fault primitives (response/error/delay/drop) with canonical status codes
# and body shapes that real clients handle gracefully.
#
# Usage:
#     load("./recipes/http2.star", "rate_limited", "server_error")
#
#     faulty = fault_assumption("faulty_api",
#         target = api.main,
#         rules  = [rate_limited(path = "/api/**")],
#     )

# rate_limited returns 429 with a Retry-After hint. Use to verify clients
# honor the rate-limit contract.
def rate_limited(path = "/*"):
    return response(
        path = path,
        status = 429,
        body = "{\"error\":\"rate limited\",\"retry_after\":\"1s\"}",
    )

# server_error returns 500 — the generic "something went wrong" failure.
def server_error(path = "/*"):
    return error(
        path = path,
        status = 500,
        message = "internal server error",
    )

# service_unavailable returns 503. Different from 500: retryable per HTTP
# semantics, clients should back off and retry.
def service_unavailable(path = "/*"):
    return error(
        path = path,
        status = 503,
        message = "service unavailable",
    )

# gateway_timeout returns 504. Used to test upstream-timeout handling.
def gateway_timeout(path = "/*"):
    return error(
        path = path,
        status = 504,
        message = "gateway timeout",
    )

# slow_endpoint delays every matching request by the given duration.
def slow_endpoint(path = "/*", duration = "3s"):
    return delay(
        path = path,
        delay = duration,
    )

# maintenance_window returns 503 with Retry-After — matches the typical
# "we're deploying" response from load balancers.
def maintenance_window(path = "/*"):
    return response(
        path = path,
        status = 503,
        body = "{\"error\":\"maintenance\",\"retry_after\":\"60s\"}",
    )

# stream_reset closes the HTTP/2 stream mid-request (RST_STREAM equivalent).
# Triggers the client's stream-level error path.
def stream_reset(path = "/*"):
    return drop(path = path)

# flaky returns server errors a fraction of the time. Good for retry tests.
def flaky(path = "/*", probability = "20%"):
    return error(
        path = path,
        status = 500,
        message = "flaky endpoint",
        probability = probability,
    )

# unauthorized returns 401. Verifies auth-error handling and token refresh.
def unauthorized(path = "/*"):
    return error(
        path = path,
        status = 401,
        message = "unauthorized",
    )

# forbidden returns 403. Tests authorization (vs authentication) failures.
def forbidden(path = "/*"):
    return error(
        path = path,
        status = 403,
        message = "forbidden",
    )
