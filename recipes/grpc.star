# Faultbox recipes: gRPC
#
# Per RFC-018: one namespace struct per recipe file.
# Per RFC-019: stdlib ships embedded in the faultbox binary; load via the
# @faultbox/ prefix.
#
# Usage:
#     load("@faultbox/recipes/grpc.star", "grpc")
#
#     unstable_api = fault_assumption("unstable_api",
#         target = api.main,
#         rules  = [grpc.unavailable(method = "/pkg.Service/Method")],
#     )
#
# Scope: canonical gRPC status codes (google.rpc.Code 1-16). The proxy
# matches rules by gRPC method path (e.g. "/pkg.Service/Method"); the
# status code and message below are what the gRPC client receives
# verbatim. Client-side status.FromError / switch-on-code handlers
# recognize them by code, so retry policies behave identically to
# production failures.

grpc = struct(
    # unavailable — code 14 (UNAVAILABLE). The most retried gRPC error.
    # Server is transiently down or load-shedding; clients are expected
    # to retry with backoff. Exposes retry-policy regressions and
    # circuit-breaker thresholds.
    unavailable = lambda method = "*": error(
        method = method,
        status = 14,
        message = "server unavailable, retry",
    ),

    # deadline_exceeded — code 4 (DEADLINE_EXCEEDED). Server missed the
    # client's per-call deadline. Tests client deadline propagation and
    # downstream cancellation.
    deadline_exceeded = lambda method = "*": error(
        method = method,
        status = 4,
        message = "deadline exceeded",
    ),

    # resource_exhausted — code 8 (RESOURCE_EXHAUSTED). gRPC analog of
    # HTTP 429: quota hit, rate limit exceeded, or inflight-request
    # cap reached. Drivers surface quota-specific retry decisions.
    resource_exhausted = lambda method = "*": error(
        method = method,
        status = 8,
        message = "resource exhausted — rate limit or quota",
    ),

    # unauthenticated — code 16 (UNAUTHENTICATED). Client credential
    # missing / invalid / expired. Token-refresh and re-auth flows.
    unauthenticated = lambda method = "*": error(
        method = method,
        status = 16,
        message = "unauthenticated",
    ),

    # permission_denied — code 7 (PERMISSION_DENIED). Caller identity
    # is known but lacks authorization. Distinct from unauthenticated;
    # retry with fresh credentials will not help.
    permission_denied = lambda method = "*": error(
        method = method,
        status = 7,
        message = "permission denied",
    ),

    # internal — code 13 (INTERNAL). Generic server-side failure,
    # "this should never happen." Non-retryable by default; tests
    # client error surfacing to user + alerting paths.
    internal = lambda method = "*": error(
        method = method,
        status = 13,
        message = "internal server error",
    ),

    # not_found — code 5 (NOT_FOUND). The target resource does not
    # exist. Surfaces 404-handling logic in clients that treat missing
    # resources as a normal case (versus an error).
    not_found = lambda method = "*": error(
        method = method,
        status = 5,
        message = "not found",
    ),

    # aborted — code 10 (ABORTED). Transactional operation aborted,
    # typically due to a concurrency conflict. Clients MAY retry at a
    # higher level; the canonical "optimistic concurrency" failure.
    aborted = lambda method = "*": error(
        method = method,
        status = 10,
        message = "aborted — transactional conflict",
    ),

    # slow_method — delays the gRPC response by `duration`. Tests
    # client deadline handling without tripping DEADLINE_EXCEEDED
    # server-side.
    slow_method = lambda method = "*", duration = "3s": delay(
        method = method,
        delay = duration,
    ),

    # connection_drop — closes the TCP connection mid-call. Forces
    # client reconnect through the resolver + subchannel paths.
    connection_drop = lambda method = "*": drop(method = method),
)
