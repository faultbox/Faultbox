# @faultbox/mocks/jwt.star
#
# Auto-generated EdDSA JWT issuer with a JWKS endpoint. Saves customers
# from re-implementing the same Go-side jwtgen tool every time they
# stand up a Faultbox spec for an HTTP-auth service. Customer ask B3
# from the inDrive feedback analysis (v0.9.9).
#
# Usage:
#
#     load("@faultbox/mocks/jwt.star", "jwt")
#
#     auth = jwt.server(
#         name      = "auth",
#         interface = interface("main", "http", 8090),
#         issuer    = "https://auth.example.com",
#     )
#
#     def test_authorised_request():
#         token = auth.sign(claims = {
#             "sub":   "user-42",
#             "email": "alice@example.com",
#             "scope": "read:orders",
#         })
#         result = step(api.public, "get",
#                       path    = "/orders",
#                       headers = {"Authorization": "Bearer " + token})
#         assert_true(result.status_code == 200)
#
# What `jwt.server(...)` returns is a struct with three attrs:
#   .service     → ServiceDef (use as fault target / depends_on)
#   .sign(claims) → mints a token signed by the issuer's private key
#   .jwks        → the published JWKS dict (rarely needed; use it if
#                  you want to compose a custom OIDC discovery layout)
#
# Routes the mock service exposes:
#   GET /.well-known/jwks.json
#   GET /jwks
#   GET /.well-known/openid-configuration
#
# Algorithm: Ed25519 (EdDSA). Modern OIDC issuers (Auth0, Okta,
# Keycloak with EdDSA enabled) accept this; older RSA/HS-only stacks
# need a different mock — file an issue if you hit one.

def _server(name, interface, issuer = "https://faultbox-auth.invalid", key_id = "k1", depends_on = []):
    keypair = jwt_keypair(kid = key_id)
    jwks_body = jwt_jwks(keypair = keypair)
    discovery = {
        "issuer":                                issuer,
        "jwks_uri":                              issuer + "/.well-known/jwks.json",
        "id_token_signing_alg_values_supported": ["EdDSA"],
        "subject_types_supported":               ["public"],
        "response_types_supported":              ["id_token"],
    }

    svc = mock_service(
        name,
        interface,
        depends_on = depends_on,
        routes = {
            "GET /.well-known/jwks.json":              json_response(body = jwks_body),
            "GET /jwks":                                json_response(body = jwks_body),
            "GET /.well-known/openid-configuration":   json_response(body = discovery),
        },
    )

    return struct(
        service = svc,
        sign    = lambda claims = {}: jwt_sign(keypair = keypair, claims = claims),
        jwks    = jwks_body,
    )

jwt = struct(
    # Primary constructor — auto-generates an EdDSA keypair and
    # exposes a JWKS-publishing mock_service.
    server = _server,

    # Lower-level escape hatches for customers who want to assemble
    # their own discovery layout. Each call generates a fresh keypair.
    keypair = lambda kid = "k1": jwt_keypair(kid = kid),
    sign    = lambda keypair, claims = {}: jwt_sign(keypair = keypair, claims = claims),
    jwks    = lambda keypair: jwt_jwks(keypair = keypair),
)
