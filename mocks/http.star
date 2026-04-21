# @faultbox/mocks/http.star
#
# OpenAPI-driven HTTP mocks. Thin Starlark wrapper over
# mock_service(openapi=...) that takes an OpenAPI 3.0 document and
# auto-generates routes from paths × operations, serving the first
# declared example for each one.
#
# RFC-021. Target: v0.9.3.
#
# Usage:
#
#     load("@faultbox/mocks/http.star", "http")
#
#     auth = http.server(
#         name      = "auth",
#         interface = interface("main", "http", 8090),
#         openapi   = "./specs/auth.openapi.yaml",
#     )
#
# Every path × method in the OpenAPI document becomes a mock route
# whose response body is the first `example:` (or first entry in
# `examples:`) declared on the operation's first 2xx response. If
# neither is present, Faultbox refuses to start — Phase 1 requires
# explicit examples. Phase 3 will add schema synthesis as a fallback.
#
# External `$ref` to http://... is rejected at load time. Filesystem
# refs are resolved relative to the OpenAPI document's directory.
#
# Power users can still supply `routes=` and `default=` alongside
# `openapi=` to pin specific operations or override the 404 response.

def _server(name, interface, openapi, examples = "first", validate = "off", overrides = None, depends_on = [], tls = False, default = None, routes = None):
    kwargs = {
        "openapi":    openapi,
        "examples":   examples,
        "validate":   validate,
        "depends_on": depends_on,
        "tls":        tls,
    }
    if overrides != None:
        kwargs["overrides"] = overrides
    if default != None:
        kwargs["default"] = default
    if routes != None:
        kwargs["routes"] = routes
    return mock_service(name, interface, **kwargs)

http = struct(
    # Primary constructor — OpenAPI-backed HTTP mock.
    server = _server,

    # Response constructors re-exported for callers who want to pin
    # specific routes alongside the generated ones (or override the
    # default 404). These are the same builtins available globally;
    # re-exporting keeps a single import path for the mock-writer.
    json        = lambda status = 200, body = None, headers = {}: json_response(status = status, body = body, headers = headers),
    text        = lambda status = 200, body = "", headers = {}: text_response(status = status, body = body, headers = headers),
    status_only = lambda code: status_only(code = code),
    redirect    = lambda location, status = 302: redirect(location = location, status = status),
    dynamic     = lambda fn: dynamic(fn = fn),
)
