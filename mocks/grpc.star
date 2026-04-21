# @faultbox/mocks/grpc.star
#
# Typed-proto gRPC mocks for SUTs with compiled *.pb.go stubs.
# Thin Starlark wrapper over mock_service(descriptors=...) that takes a
# FileDescriptorSet (protoc-generated .pb file) and a method-to-response
# map, then lets the gRPC handler encode responses as the customer's
# real message types at request time.
#
# RFC-023. Target: v0.9.0.
#
# Usage:
#
#     load("@faultbox/mocks/grpc.star", "grpc")
#
#     geo_config = grpc.server(
#         name        = "geo-config",
#         interface   = interface("main", "grpc", 9001),
#         descriptors = "./proto/all_upstreams.pb",
#         services    = {
#             "/inDriver.geo_config.GeoConfigService/GetCity": {
#                 "response": {
#                     "id":       42,
#                     "name":     "Almaty",
#                     "country":  "KZ",
#                     "currency": "KZT",
#                 },
#             },
#             "/inDriver.geo_config.GeoConfigService/AdminUpdate": {
#                 "error": {"code": "PERMISSION_DENIED", "message": "admin only"},
#             },
#             # Dynamic handler — receives the typed request as a dict.
#             "/inDriver.geo_config.GeoConfigService/GetCityByCoords":
#                 grpc.dynamic(lambda req: grpc.response({"id": 1, "name": "north"})),
#             # Raw wire bytes for exotic cases (oneofs, extensions).
#             "/inDriver.geo_config.GeoConfigService/Exotic":
#                 grpc.raw_response(b"\\x08\\x2a"),
#         },
#     )
#
# Generate `all_upstreams.pb` with:
#
#     protoc --include_imports --descriptor_set_out=all_upstreams.pb \
#            proto/inDriver/**/*.proto
#
# Faultbox does NOT ship or wrap protoc — customers own their proto
# build pipeline (resolved question 3 in RFC-023).
#
# Each service-map value is one of:
#   - {"response": <dict>}         — happy-path typed response
#   - {"error":    <dict>}         — status-code error; dict has
#                                     "code" (string or int) + "message"
#   - grpc.dynamic(fn)             — per-request Starlark handler
#   - grpc.raw_response(bytes)     — escape hatch, pre-encoded wire bytes

def _server(name, interface, descriptors, services = {}, depends_on = [], tls = False):
    # Walk the services map and convert each value into a mock_response
    # compatible with the routes= kwarg on mock_service().
    routes = {}
    for method_path, spec in services.items():
        # A dict-type spec is either {"response": ...} or {"error": ...}.
        # Everything else is already a mock_response value from
        # grpc.dynamic() / grpc.raw_response().
        if type(spec) == "dict":
            if "response" in spec:
                routes[method_path] = grpc_typed_response(body = spec["response"])
            elif "error" in spec:
                err = spec["error"]
                code = err.get("code", "INTERNAL")
                message = err.get("message", "")
                routes[method_path] = grpc_error(code = code, message = message)
            else:
                fail("grpc.server: service %s must have 'response' or 'error' key (got %s)" % (method_path, spec))
        else:
            # grpc.dynamic() or grpc.raw_response() — pass through as-is.
            routes[method_path] = spec

    return mock_service(
        name,
        interface,
        descriptors = descriptors,
        routes      = routes,
        depends_on  = depends_on,
        tls         = tls,
    )

grpc = struct(
    # Primary constructor — typed gRPC mock backed by a FileDescriptorSet.
    server = _server,

    # Response constructors.
    # grpc.response(body=dict) — typed response, encoded at request time.
    response = lambda body = {}: grpc_typed_response(body = body),
    # grpc.raw_response(body=bytes) — pre-encoded wire bytes escape hatch.
    raw_response = lambda body: grpc_raw_response(body = body),
    # grpc.error(code, message) — status-code error. code is the canonical
    # name ("UNAVAILABLE") or integer.
    error = lambda code, message = "": grpc_error(code = code, message = message),
    # grpc.dynamic(fn) — per-request handler; fn receives a request dict
    # and must return one of grpc.response / grpc.error / grpc.raw_response.
    dynamic = lambda fn: dynamic(fn = fn),
)
